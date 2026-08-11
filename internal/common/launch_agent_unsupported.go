//go:build !darwin

package common

import "context"

type unsupportedManager struct{}

// NewLaunchAgentManager returns a manager whose lifecycle operations provide exact
// foreground guidance without invoking the supplied runner.
func NewLaunchAgentManager(LaunchAgentRunner) (LaunchAgentManager, error) {
	return unsupportedManager{}, nil
}

func (unsupportedManager) Install(context.Context, LaunchAgentConfig) error {
	return ErrLaunchAgentUnsupported
}
func (unsupportedManager) Status(context.Context) (LaunchAgentStatus, error) {
	return LaunchAgentStatus{}, ErrLaunchAgentUnsupported
}
func (unsupportedManager) Restart(context.Context) error   { return ErrLaunchAgentUnsupported }
func (unsupportedManager) Stop(context.Context) error      { return ErrLaunchAgentUnsupported }
func (unsupportedManager) Uninstall(context.Context) error { return ErrLaunchAgentUnsupported }
