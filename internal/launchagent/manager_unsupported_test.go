//go:build !darwin

package launchagent

import (
	"context"
	"errors"
	"testing"
)

type panicRunner struct{}

func (panicRunner) Run(context.Context, string, ...string) ([]byte, error) {
	panic("unsupported manager invoked runner")
}

func TestUnsupportedManagerReturnsExactForegroundGuidanceWithoutRunner(t *testing.T) {
	manager, err := NewManager(panicRunner{})
	if err != nil {
		t.Fatal(err)
	}
	operations := []struct {
		name string
		run  func() error
	}{
		{name: "install", run: func() error { return manager.Install(context.Background(), Config{}) }},
		{name: "status", run: func() error { _, err := manager.Status(context.Background()); return err }},
		{name: "restart", run: func() error { return manager.Restart(context.Background()) }},
		{name: "stop", run: func() error { return manager.Stop(context.Background()) }},
		{name: "uninstall", run: func() error { return manager.Uninstall(context.Background()) }},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			err := operation.run()
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("error = %v, want ErrUnsupported", err)
			}
			if err.Error() != ForegroundGuidance {
				t.Fatalf("error = %q, want exact guidance %q", err, ForegroundGuidance)
			}
		})
	}
}
