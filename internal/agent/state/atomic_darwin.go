//go:build darwin

package state

import "golang.org/x/sys/unix"

func atomicRenameNoReplace(oldDir int, oldName string, newDir int, newName string) error {
	return unix.RenameatxNp(oldDir, oldName, newDir, newName, unix.RENAME_EXCL)
}

func atomicRenameExchange(leftDir int, leftName string, rightDir int, rightName string) error {
	return unix.RenameatxNp(leftDir, leftName, rightDir, rightName, unix.RENAME_SWAP)
}
