//go:build unix

package vault

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// checkPrivateDir requires dir to be a directory owned by this user with no group or other access.
func checkPrivateDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrInsecureHome, dir)
	}
	if !ownedByCurrentUser(info) {
		return fmt.Errorf("%w: %s is owned by another user; point ADDN_HOME at a directory you own", ErrInsecureHome, dir)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%w: %s has mode %o; run chmod 700 %s", ErrInsecureHome, dir, perm, dir)
	}
	return nil
}

// checkPrivateFile requires path, if present, to be a non-linked regular file owned by this user, mode 0600 or stricter.
func checkPrivateFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrInsecureHome, path)
	}
	if !ownedByCurrentUser(info) {
		return fmt.Errorf("%w: %s is owned by another user", ErrInsecureHome, path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%w: %s has mode %o; run chmod 600 %s", ErrInsecureHome, path, perm, path)
	}
	return nil
}

func createPrivateFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
}

// openKeyFile opens the key file inside home without following a link or blocking on a FIFO or device.
func openKeyFile(home string) (*os.File, error) {
	path := filepath.Join(home, keyFileName)
	dir, err := unix.Open(home, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: home, Err: err}
	}
	defer unix.Close(dir)
	fd, err := unix.Openat(dir, keyFileName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.EMLINK):
		return nil, fmt.Errorf("%w: %s is a symbolic link", ErrInsecureKeyFile, path)
	case err != nil:
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func checkKeyFileInfo(info fs.FileInfo, path string) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrInsecureKeyFile, path)
	}
	if !ownedByCurrentUser(info) {
		return fmt.Errorf("%w: %s is owned by another user", ErrInsecureKeyFile, path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%w: %s has mode %o; run chmod 600 %s", ErrInsecureKeyFile, path, perm, path)
	}
	return nil
}

func ownedByCurrentUser(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
