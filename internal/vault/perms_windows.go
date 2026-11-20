//go:build windows

package vault

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// On Windows the data dir lives in the per-user profile and relies on its inherited ACLs.

func checkPrivateDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrInsecureHome, dir)
	}
	return nil
}

func checkPrivateFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrInsecureHome, path)
	}
	return nil
}

func createPrivateFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
}

func openKeyFile(home string) (*os.File, error) {
	path := filepath.Join(home, keyFileName)
	if info, err := os.Lstat(path); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s is a symbolic link", ErrInsecureKeyFile, path)
	}
	return os.Open(path)
}

func checkKeyFileInfo(info fs.FileInfo, path string) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrInsecureKeyFile, path)
	}
	return nil
}

func syncDir(string) error { return nil }
