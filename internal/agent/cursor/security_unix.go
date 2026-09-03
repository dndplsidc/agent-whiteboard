//go:build unix

package cursor

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// validateCursorExecutable performs the adapter-owned checks that are stricter
// than the generic launcher contract. In particular, all checks use lstat so a
// path that names a symlink is never accepted by the Cursor adapter.
func validateCursorExecutable(path string) error {
	if !absolute(path) {
		return errors.New("unsafe Cursor executable")
	}
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		return errors.New("unsafe Cursor executable")
	}
	if err := validateCursorExecutableStat(&stat); err != nil {
		return err
	}
	return nil
}

func validateCursorExecutableStat(stat *unix.Stat_t) error {
	if stat == nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) || uint64(stat.Nlink) != 1 {
		return errors.New("unsafe Cursor executable")
	}
	permissions := stat.Mode & 0o777
	if permissions&0o111 == 0 || permissions&0o022 != 0 {
		return errors.New("unsafe Cursor executable")
	}
	return nil
}

// sameCursorStatTimes uses the POSIX nanosecond modification and metadata
// change clocks exposed by both Darwin and Linux unix.Stat_t values. Keeping
// this comparison in the Unix security layer avoids treating a same-size
// in-place rewrite as the original image.
func sameCursorStatTimes(left, right *unix.Stat_t) bool {
	return left != nil && right != nil && left.Mtim.Sec == right.Mtim.Sec && left.Mtim.Nsec == right.Mtim.Nsec && left.Ctim.Sec == right.Ctim.Sec && left.Ctim.Nsec == right.Ctim.Nsec
}
