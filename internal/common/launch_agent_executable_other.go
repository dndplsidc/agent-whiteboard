//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package common

import (
	"errors"
	"os"
)

func currentUserExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0o111 == 0 {
		return errors.New("path must identify an executable file")
	}
	return nil
}
