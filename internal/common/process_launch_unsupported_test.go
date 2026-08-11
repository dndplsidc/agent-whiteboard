//go:build !darwin && !linux

package common

import (
	"context"
	"testing"
)

func TestLaunchIsExplicitlyUnsupported(t *testing.T) {
	child, err := NewProcessGroupLauncher().Launch(context.Background(), ProcessRequest{})
	if child != nil {
		t.Fatal("unsupported launch returned a child")
	}
	processError, ok := err.(*ProcessError)
	if !ok || processError.Kind != ProcessErrorUnsupported {
		t.Fatalf("expected unsupported-platform error, got %T: %v", err, err)
	}
}
