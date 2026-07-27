//go:build darwin

package processgroup

import (
	"errors"

	"golang.org/x/sys/unix"
)

type processIdentity struct {
	kqueue int
}

func newProcessIdentity(pid int) (processIdentity, error) {
	queue, err := unix.Kqueue()
	if err != nil {
		return processIdentity{}, err
	}
	change := unix.Kevent_t{}
	unix.SetKevent(&change, pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	change.Fflags = unix.NOTE_EXIT
	if _, err := unix.Kevent(queue, []unix.Kevent_t{change}, nil, nil); err != nil {
		_ = unix.Close(queue)
		return processIdentity{}, err
	}
	return processIdentity{kqueue: queue}, nil
}

func (identity processIdentity) waitForExit() error {
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
			return nil
		}
	}
}

func (identity processIdentity) close() error { return unix.Close(identity.kqueue) }
