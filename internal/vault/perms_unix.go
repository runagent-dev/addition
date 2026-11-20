//go:build unix

package vault

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// homeDir is the validated data dir. SQLite reaches it by path, which is safe because no other
// user can replace any component of it; vault-owned files are reached through fd alone.
type homeDir struct {
	path string
	fd   int
}

// openHome resolves the data dir to a symlink-free path no other user can redirect, and opens it.
func openHome(env Env, create bool) (*homeDir, error) {
	home, err := ResolveHome(env)
	if err != nil {
		return nil, err
	}
	if err := checkInsideProfile(env, home); err != nil {
		return nil, err
	}
	if create {
		if err := os.MkdirAll(home, 0o700); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}
	resolved, err := filepath.EvalSymlinks(home)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, notInitializedAt(home)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve data dir: %w", err)
	}
	if err := checkInsideProfile(env, resolved); err != nil {
		return nil, err
	}
	if err := checkAncestors(resolved); err != nil {
		return nil, err
	}
	fd, err := unix.Open(resolved, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot open %s as a directory: %v", ErrInsecureHome, resolved, err)
	}
	h := &homeDir{path: resolved, fd: fd}
	if err := h.checkPrivate(); err != nil {
		h.Close()
		return nil, err
	}
	return h, nil
}

func (h *homeDir) Close() { _ = unix.Close(h.fd) }

// checkAncestors applies OpenSSH's StrictModes rule to every directory above dir, from / down.
func checkAncestors(dir string) error {
	var ancestors []string
	for p := filepath.Dir(dir); ; p = filepath.Dir(p) {
		ancestors = append(ancestors, p)
		if p == filepath.Dir(p) {
			break
		}
	}
	for i := len(ancestors) - 1; i >= 0; i-- {
		if err := checkAncestor(ancestors[i]); err != nil {
			return err
		}
	}
	return nil
}

func checkAncestor(dir string) error {
	var st unix.Stat_t
	if err := unix.Lstat(dir, &st); err != nil {
		return &fs.PathError{Op: "lstat", Path: dir, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("%w: %s is not a directory", ErrInsecureHome, dir)
	}
	rootOwned := st.Uid == 0
	if !rootOwned && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%w: %s is owned by another user, who could replace the data dir", ErrInsecureHome, dir)
	}
	stickyRoot := rootOwned && st.Mode&unix.S_ISVTX != 0
	if st.Mode&0o022 != 0 && !stickyRoot {
		return fmt.Errorf("%w: %s is writable by other users, who could replace the data dir; move ADDN_HOME or run chmod go-w %s", ErrInsecureHome, dir, dir)
	}
	return nil
}

func (h *homeDir) checkPrivate() error {
	var st unix.Stat_t
	if err := unix.Fstat(h.fd, &st); err != nil {
		return &fs.PathError{Op: "fstat", Path: h.path, Err: err}
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%w: %s is owned by another user; point ADDN_HOME at a directory you own", ErrInsecureHome, h.path)
	}
	if perm := st.Mode & 0o777; perm&0o077 != 0 {
		return fmt.Errorf("%w: %s has mode %o; run chmod 700 %s", ErrInsecureHome, h.path, perm, h.path)
	}
	return nil
}

func (h *homeDir) join(name string) string { return filepath.Join(h.path, name) }

// checkPrivateFile requires name, if present, to be a non-linked regular file owned by this user, mode 0600 or stricter.
func (h *homeDir) checkPrivateFile(name string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(h.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &fs.PathError{Op: "lstat", Path: h.join(name), Err: err}
	}
	return checkPrivateStat(&st, h.join(name), ErrInsecureHome)
}

func checkPrivateStat(st *unix.Stat_t, path string, kind error) error {
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("%w: %s is not a regular file", kind, path)
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%w: %s is owned by another user", kind, path)
	}
	if perm := st.Mode & 0o777; perm&0o077 != 0 {
		return fmt.Errorf("%w: %s has mode %o; run chmod 600 %s", kind, path, perm, path)
	}
	return nil
}

func (h *homeDir) createPrivateFile(name string) (*os.File, error) {
	fd, err := unix.Openat(h.fd, name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, &fs.PathError{Op: "create", Path: h.join(name), Err: err}
	}
	return os.NewFile(uintptr(fd), h.join(name)), nil
}

// openKeyFile opens the key file without following a link or blocking on a FIFO or device.
func (h *homeDir) openKeyFile() (*os.File, error) {
	path := h.join(keyFileName)
	fd, err := unix.Openat(h.fd, keyFileName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.EMLINK):
		return nil, fmt.Errorf("%w: %s is a symbolic link", ErrInsecureKeyFile, path)
	case err != nil:
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		f.Close()
		return nil, &fs.PathError{Op: "fstat", Path: path, Err: err}
	}
	if err := checkPrivateStat(&st, path, ErrInsecureKeyFile); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// beforeKeyPublish lets tests hold a written key file just before it is published.
var beforeKeyPublish = func() {}

const keyTempPrefix = "." + keyFileName + "-"

// writeKeyFile publishes a complete, synced key file under its final name, never replacing one.
func (h *homeDir) writeKeyFile(content string) error {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	tmp := keyTempPrefix + hex.EncodeToString(suffix[:])
	f, err := h.createPrivateFile(tmp)
	if err != nil {
		return fmt.Errorf("create key file: %w", err)
	}
	defer func() { _ = unix.Unlinkat(h.fd, tmp, 0) }()
	_, err = f.WriteString(content)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write key file: %w", err)
	}
	beforeKeyPublish()
	if err := h.renameNoReplace(tmp, keyFileName); err != nil {
		return fmt.Errorf("publish key file: %w", &fs.PathError{Op: "rename", Path: h.join(keyFileName), Err: err})
	}
	return h.sync()
}

// linkNoReplace publishes by hard link, which fails if newName exists, then drops oldName.
func (h *homeDir) linkNoReplace(oldName, newName string) error {
	if err := unix.Linkat(h.fd, oldName, h.fd, newName, 0); err != nil {
		return err
	}
	return unix.Unlinkat(h.fd, oldName, 0)
}

// removeKeyTemps drops temporary key names an interrupted publication left, keeping anything not ours.
// Callers hold the writer lock, so no publication is in flight.
func (h *homeDir) removeKeyTemps() error {
	dir, err := unix.Openat(h.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return &fs.PathError{Op: "open", Path: h.path, Err: err}
	}
	listing := os.NewFile(uintptr(dir), h.path)
	names, err := listing.Readdirnames(-1)
	listing.Close()
	if err != nil {
		return fmt.Errorf("list data dir: %w", err)
	}
	removed := false
	for _, name := range names {
		if !strings.HasPrefix(name, keyTempPrefix) || h.checkOwnedRegular(name) != nil {
			continue
		}
		if err := unix.Unlinkat(h.fd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return &fs.PathError{Op: "remove", Path: h.join(name), Err: err}
		}
		removed = true
	}
	if removed {
		return h.sync()
	}
	return nil
}

func (h *homeDir) checkOwnedRegular(name string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(h.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || int(st.Uid) != os.Geteuid() {
		return fs.ErrPermission
	}
	return nil
}

func (h *homeDir) sync() error {
	if err := unix.Fsync(h.fd); err != nil {
		return fmt.Errorf("sync data dir: %w", err)
	}
	return nil
}
