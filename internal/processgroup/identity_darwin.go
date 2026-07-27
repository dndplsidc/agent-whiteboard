//go:build darwin

package processgroup

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

type osProcessIdentity struct {
	kqueue int
	pid    int
}

func openProcessIdentity(pid int) (osProcessIdentity, error) {
	queue, err := unix.Kqueue()
	if err != nil {
		return osProcessIdentity{}, err
	}
	change := unix.Kevent_t{}
	unix.SetKevent(&change, pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	change.Fflags = unix.NOTE_EXIT
	if _, err := unix.Kevent(queue, []unix.Kevent_t{change}, nil, nil); err != nil {
		_ = unix.Close(queue)
		return osProcessIdentity{}, err
	}
	return osProcessIdentity{kqueue: queue, pid: pid}, nil
}

func (identity osProcessIdentity) waitForExit() error {
	events := make([]unix.Kevent_t, 1)
	for {
		count, err := unix.Kevent(identity.kqueue, nil, events, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if count == 1 {
			return validateExitEvent(events[0], identity.pid)
		}
	}
}

func validateExitEvent(event unix.Kevent_t, pid int) error {
	if event.Flags&unix.EV_ERROR != 0 {
		if event.Data != 0 {
			return syscall.Errno(event.Data)
		}
		return syscall.EIO
	}
	if event.Ident != uint64(pid) {
		return fmt.Errorf("unexpected process identity %d (want %d)", event.Ident, pid)
	}
	if event.Filter != unix.EVFILT_PROC {
		return fmt.Errorf("unexpected kqueue filter %d", event.Filter)
	}
	if event.Fflags&unix.NOTE_EXIT == 0 {
		return fmt.Errorf("process event missing NOTE_EXIT: %#x", event.Fflags)
	}
	return nil
}

func (identity osProcessIdentity) close() error { return unix.Close(identity.kqueue) }

// Darwin returns EPERM when an exited, unreaped leader is the process group's
// only remaining member. A signalable descendant would make kill(2) succeed.
func isExitedLeaderOnlyGroupError(err error) bool { return errors.Is(err, unix.EPERM) }
