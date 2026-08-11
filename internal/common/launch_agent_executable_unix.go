//go:build aix || android || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package common

import (
	"errors"

	"golang.org/x/sys/unix"
)

func currentUserExecutable(path string) error {
	if err := unix.Access(path, unix.X_OK); err != nil {
		return errors.New("path must identify a file executable by the current user")
	}
	return nil
}
