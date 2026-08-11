//go:build !darwin && !linux

package common

import (
	"context"
	"errors"
	"runtime"
)

// Launch reports that isolated process groups are unavailable on this
// platform. It never attempts to start the requested executable.
func (*ProcessGroupLauncher) Launch(context.Context, ProcessRequest) (ManagedProcess, error) {
	return nil, processError(ProcessErrorUnsupported, "launch", errors.New("process groups are unsupported on "+runtime.GOOS))
}
