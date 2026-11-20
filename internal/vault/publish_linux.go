package vault

import (
	"errors"

	"golang.org/x/sys/unix"
)

func (h *homeDir) renameNoReplace(oldName, newName string) error {
	err := unix.Renameat2(h.fd, oldName, h.fd, newName, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
		return h.linkNoReplace(oldName, newName)
	}
	return err
}
