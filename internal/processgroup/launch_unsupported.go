//go:build !darwin && !linux

package processgroup

import (
	"context"
	"errors"
	"runtime"

	"github.com/edocsss/agent-whiteboard/internal/provider"
)

// Launch reports that isolated process groups are unavailable on this
// platform. It never attempts to start the requested executable.
func (*Launcher) Launch(context.Context, provider.LaunchRequest) (provider.ManagedChild, error) {
	return nil, processError(ErrorUnsupported, "launch", errors.New("process groups are unsupported on "+runtime.GOOS))
}
