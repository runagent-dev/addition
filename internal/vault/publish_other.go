//go:build unix && !linux && !darwin

package vault

func (h *homeDir) renameNoReplace(oldName, newName string) error {
	return h.linkNoReplace(oldName, newName)
}
