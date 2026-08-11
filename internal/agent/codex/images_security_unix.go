//go:build darwin || linux

package codex

import (
	"os"
	"syscall"
)

func secureImageFile(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}
