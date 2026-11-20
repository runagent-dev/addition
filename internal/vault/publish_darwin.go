package vault

import (
	"errors"

	"golang.org/x/sys/unix"
)

func (h *homeDir) renameNoReplace(oldName, newName string) error {
	err := unix.RenameatxNp(h.fd, oldName, h.fd, newName, unix.RENAME_EXCL)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EINVAL) {
		return h.linkNoReplace(oldName, newName)
	}
	return err
}
