//go:build !darwin

package launchagent

import "context"

type unsupportedManager struct{}

// NewManager returns a manager whose lifecycle operations provide exact
// foreground guidance without invoking the supplied runner.
func NewManager(Runner) (Manager, error) {
	return unsupportedManager{}, nil
}

func (unsupportedManager) Install(context.Context, Config) error  { return ErrUnsupported }
func (unsupportedManager) Status(context.Context) (Status, error) { return Status{}, ErrUnsupported }
func (unsupportedManager) Restart(context.Context) error          { return ErrUnsupported }
func (unsupportedManager) Stop(context.Context) error             { return ErrUnsupported }
func (unsupportedManager) Uninstall(context.Context) error        { return ErrUnsupported }
