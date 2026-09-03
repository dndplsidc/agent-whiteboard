//go:build unix

package cursor

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestValidateCursorExecutableRejectsUnsafeMetadata(t *testing.T) {
	base := unix.Stat_t{Mode: unix.S_IFREG | 0o700, Uid: uint32(os.Geteuid()), Nlink: 1}
	cases := []struct {
		name string
		edit func(*unix.Stat_t)
	}{
		{name: "wrong owner", edit: func(stat *unix.Stat_t) { stat.Uid++ }},
		{name: "multiple links", edit: func(stat *unix.Stat_t) { stat.Nlink = 2 }},
		{name: "not executable", edit: func(stat *unix.Stat_t) { stat.Mode = unix.S_IFREG | 0o600 }},
		{name: "group writable", edit: func(stat *unix.Stat_t) { stat.Mode |= 0o020 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stat := base
			tc.edit(&stat)
			if err := validateCursorExecutableStat(&stat); err == nil {
				t.Fatal("unsafe executable metadata accepted")
			}
		})
	}
}
