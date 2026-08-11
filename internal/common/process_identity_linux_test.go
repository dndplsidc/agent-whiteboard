//go:build linux

package common

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestPollExitEventRejectsInvalidDescriptor(t *testing.T) {
	exited, err := pollExitEvent(unix.POLLNVAL)
	require.False(t, exited)
	require.ErrorIs(t, err, unix.EBADF)
}

func TestPollExitEventRecognizesLeaderExit(t *testing.T) {
	exited, err := pollExitEvent(unix.POLLIN)
	require.NoError(t, err)
	require.True(t, exited)
}
