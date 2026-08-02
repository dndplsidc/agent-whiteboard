package whiteboard

import (
	"context"
	"errors"
)

// UncertainCreateError reports that Create rollback could not verify that the
// generated resource ID is absent. Callers must treat that ID as possibly live.
type UncertainCreateError interface {
	error
	ResourceMayExist() bool
}

func createMayHaveCommitted(err error) bool {
	var uncertain UncertainCreateError
	return errors.As(err, &uncertain) && uncertain.ResourceMayExist()
}

// Store persists whiteboards. Create errors leave no resource behind unless
// the error implements UncertainCreateError.
type Store interface {
	Create(context.Context, Whiteboard) error
	Get(context.Context, string) (Whiteboard, error)
	Replace(context.Context, Whiteboard) error
	Delete(context.Context, string) error
	Ready(context.Context) error
	Close() error
}
