//go:build darwin

package common

import (
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestObservedLeaderExitAllowsDarwinLeaderOnlyEPERMCleanupRetry(t *testing.T) {
	monitorReady := make(chan struct{})
	monitorRelease := make(chan struct{})
	originalNewProcessIdentity := newProcessIdentity
	newProcessIdentity = func(int) (processIdentity, error) {
		return fakeProcessIdentity{
			wait: func() error {
				close(monitorReady)
				<-monitorRelease
				return nil
			},
			closeFn: func() error { return syscall.EIO },
		}, nil
	}
	t.Cleanup(func() { newProcessIdentity = originalNewProcessIdentity })

	child, err := NewProcessGroupLauncher().Launch(t.Context(), helperRequest(t, "wait-stdin"))
	require.NoError(t, err)
	managed := child.(*managedChild)
	<-monitorReady

	realSignalResult := make(chan error, 1)
	managed.mu.Lock()
	managed.signalFn = func(pid int, signal unix.Signal) error {
		realSignalResult <- unix.Kill(pid, signal)
		return syscall.EIO
	}
	managed.mu.Unlock()
	close(monitorRelease)

	waitErr := child.Wait()
	require.NoError(t, <-realSignalResult)
	requireErrorKinds(t, waitErr, ProcessErrorStart, ProcessErrorSignal)
	managed.mu.Lock()
	require.Equal(t, managed.pid, managed.pgid)
	managed.signalFn = func(int, unix.Signal) error { return unix.EPERM }
	managed.mu.Unlock()

	require.NoError(t, child.Kill())
	require.Eventually(t, func() bool {
		managed.mu.Lock()
		defer managed.mu.Unlock()
		return managed.pgid == 0
	}, time.Second, time.Millisecond)
	requireProcessesGone(t, managed.pid)
}

func TestDarwinPreExitEPERMRemainsSignalFailure(t *testing.T) {
	verifiedPID := unix.Getpgrp() + 1
	child := &managedChild{
		pid:        verifiedPID,
		pgid:       verifiedPID,
		brokerPGID: unix.Getpgrp(),
		phase:      lifecycleActive,
		signalFn:   func(int, unix.Signal) error { return unix.EPERM },
	}

	err := child.Kill()
	require.Error(t, err)
	require.ErrorIs(t, err, unix.EPERM)
	var processErr *ProcessError
	require.ErrorAs(t, err, &processErr)
	require.Equal(t, ProcessErrorSignal, processErr.Kind)

	child.mu.Lock()
	defer child.mu.Unlock()
	require.Equal(t, verifiedPID, child.pgid)
	require.False(t, child.cleanupRetry)
	require.False(t, child.leaderExitObserved)
	require.NotNil(t, child.killErr)
}
