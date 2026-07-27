//go:build linux

package processgroup

import (
	"errors"

	"golang.org/x/sys/unix"
)

type osProcessIdentity struct {
	fd int
}

func openProcessIdentity(pid int) (osProcessIdentity, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return osProcessIdentity{}, err
	}
	return osProcessIdentity{fd: fd}, nil
}

func (identity osProcessIdentity) waitForExit() error {
	descriptors := []unix.PollFd{{Fd: int32(identity.fd), Events: unix.POLLIN}}
	for {
		count, err := unix.Poll(descriptors, -1)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if count > 0 {
			exited, eventErr := pollExitEvent(descriptors[0].Revents)
			if eventErr != nil {
				return eventErr
			}
			if exited {
				return nil
			}
		}
	}
}

func pollExitEvent(events int16) (bool, error) {
	if events&unix.POLLNVAL != 0 {
		return false, unix.EBADF
	}
	return events&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0, nil
}

func (identity osProcessIdentity) close() error { return unix.Close(identity.fd) }

func isExitedLeaderOnlyGroupError(error) bool { return false }
