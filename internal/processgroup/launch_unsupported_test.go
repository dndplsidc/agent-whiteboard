//go:build !darwin && !linux

package processgroup

import (
	"context"
	"testing"

	"github.com/edocsss/agent-whiteboard/internal/provider"
)

func TestLaunchIsExplicitlyUnsupported(t *testing.T) {
	child, err := NewLauncher().Launch(context.Background(), provider.LaunchRequest{})
	if child != nil {
		t.Fatal("unsupported launch returned a child")
	}
	processError, ok := err.(*Error)
	if !ok || processError.Kind != ErrorUnsupported {
		t.Fatalf("expected unsupported-platform error, got %T: %v", err, err)
	}
}
