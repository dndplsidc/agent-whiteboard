//go:build darwin

package common

import (
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestValidateExitEventRejectsKqueueError(t *testing.T) {
	event := unix.Kevent_t{Ident: 42, Filter: unix.EVFILT_PROC, Flags: unix.EV_ERROR, Fflags: unix.NOTE_EXIT, Data: int64(syscall.EBADF)}
	require.ErrorIs(t, validateExitEvent(event, 42), syscall.EBADF)
}

func TestValidateExitEventRejectsUnexpectedIdentityAndFilter(t *testing.T) {
	require.Error(t, validateExitEvent(unix.Kevent_t{Ident: 41, Filter: unix.EVFILT_PROC, Fflags: unix.NOTE_EXIT}, 42))
	require.Error(t, validateExitEvent(unix.Kevent_t{Ident: 42, Filter: unix.EVFILT_TIMER, Fflags: unix.NOTE_EXIT}, 42))
	require.Error(t, validateExitEvent(unix.Kevent_t{Ident: 42, Filter: unix.EVFILT_PROC}, 42))
}

func TestValidateExitEventAcceptsMatchingProcessExit(t *testing.T) {
	require.NoError(t, validateExitEvent(unix.Kevent_t{Ident: 42, Filter: unix.EVFILT_PROC, Fflags: unix.NOTE_EXIT}, 42))
}
