//go:build !darwin

package common

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

type panicRunner struct{}

func (panicRunner) Run(context.Context, string, ...string) ([]byte, error) {
	panic("unsupported manager invoked runner")
}

func TestUnsupportedManagerReturnsExactForegroundGuidanceWithoutRunner(t *testing.T) {
	manager, err := NewLaunchAgentManager(panicRunner{})
	if err != nil {
		t.Fatal(err)
	}
	operations := []struct {
		name string
		run  func() error
	}{
		{name: "install", run: func() error { return manager.Install(context.Background(), LaunchAgentConfig{}) }},
		{name: "status", run: func() error { _, err := manager.Status(context.Background()); return err }},
		{name: "restart", run: func() error { return manager.Restart(context.Background()) }},
		{name: "stop", run: func() error { return manager.Stop(context.Background()) }},
		{name: "uninstall", run: func() error { return manager.Uninstall(context.Background()) }},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			err := operation.run()
			if !errors.Is(err, ErrLaunchAgentUnsupported) {
				t.Fatalf("error = %v, want ErrLaunchAgentUnsupported", err)
			}
			if runtime.GOOS == "linux" {
				if err.Error() != "managed agent daemon is unsupported on linux; run 'agent-whiteboard agent serve' in the foreground" {
					t.Fatalf("error = %q, want exact Linux foreground guidance", err)
				}
			} else if strings.Contains(err.Error(), "`") || !strings.Contains(err.Error(), "unsupported on "+runtime.GOOS) {
				t.Fatalf("error = %q, want actionable %s guidance without backticks", err, runtime.GOOS)
			}
		})
	}
}
