package processgroup

import (
	"errors"
	"fmt"

	"github.com/edocsss/agent-whiteboard/internal/provider"
)

// ErrorKind classifies process-launch and process-group failures without
// requiring callers to inspect operating-system error text.
type ErrorKind string

const (
	ErrorInvalidRequest     ErrorKind = "invalid_request"
	ErrorExecutable         ErrorKind = "executable"
	ErrorWorkingDirectory   ErrorKind = "working_directory"
	ErrorStart              ErrorKind = "start"
	ErrorCanceled           ErrorKind = "canceled"
	ErrorUnsupported        ErrorKind = "unsupported_platform"
	ErrorUnsafeProcessGroup ErrorKind = "unsafe_process_group"
	ErrorSignal             ErrorKind = "signal"
)

// Error is a classified process-group operation failure.
type Error struct {
	Kind ErrorKind
	Op   string
	Err  error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err == nil {
		return fmt.Sprintf("process group %s failed (%s)", e.Op, e.Kind)
	}
	return fmt.Sprintf("process group %s failed (%s): %v", e.Op, e.Kind, e.Err)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func processError(kind ErrorKind, operation string, err error) error {
	if err == nil {
		err = errors.New("operation failed")
	}
	return &Error{Kind: kind, Op: operation, Err: err}
}

// Launcher starts provider children in isolated operating-system process
// groups. Its zero value is ready for use.
type Launcher struct{}

// NewLauncher returns an isolated provider process launcher.
func NewLauncher() *Launcher { return &Launcher{} }

var _ provider.Launcher = (*Launcher)(nil)
