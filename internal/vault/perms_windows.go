//go:build windows

package vault

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// homeDir is the data dir, which on Windows must sit in the user profile and rely on its inherited ACLs.
type homeDir struct{ path string }

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
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrInsecureHome, resolved)
	}
	return &homeDir{path: resolved}, nil
}

func (h *homeDir) Close() {}

func (h *homeDir) join(name string) string { return filepath.Join(h.path, name) }

func (h *homeDir) checkPrivateFile(name string) error {
	info, err := os.Lstat(h.join(name))
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrInsecureHome, h.join(name))
	}
	return nil
}

func (h *homeDir) createPrivateFile(name string) (*os.File, error) {
	return os.OpenFile(h.join(name), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
}

func (h *homeDir) openKeyFile() (*os.File, error) { return nil, ErrKeyFileUnsupported }

func (h *homeDir) writeKeyFile(string) error { return ErrKeyFileUnsupported }

func (h *homeDir) removeKeyTemps() error { return nil }
