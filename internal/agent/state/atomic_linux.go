//go:build linux

package state

import "golang.org/x/sys/unix"

func atomicRenameNoReplace(oldDir int, oldName string, newDir int, newName string) error {
	return unix.Renameat2(oldDir, oldName, newDir, newName, unix.RENAME_NOREPLACE)
}

func atomicRenameExchange(leftDir int, leftName string, rightDir int, rightName string) error {
	return unix.Renameat2(leftDir, leftName, rightDir, rightName, unix.RENAME_EXCHANGE)
}
