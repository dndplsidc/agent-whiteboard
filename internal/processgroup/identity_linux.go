//go:build linux

package processgroup

import (
	"errors"

	"golang.org/x/sys/unix"
)

type processIdentity struct {
	fd int
}

func newProcessIdentity(pid int) (processIdentity, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return processIdentity{}, err
	}
	return processIdentity{fd: fd}, nil
}

func (identity processIdentity) waitForExit() error {
	descriptors := []unix.PollFd{{Fd: int32(identity.fd), Events: unix.POLLIN}}
	for {
		count, err := unix.Poll(descriptors, -1)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if count > 0 && descriptors[0].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0 {
			return nil
		}
	}
}

func (identity processIdentity) close() error { return unix.Close(identity.fd) }
