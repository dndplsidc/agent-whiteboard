//go:build !darwin && !linux

package codex

import "os"

func secureImageFile(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode().Perm()&0o077 == 0
}
