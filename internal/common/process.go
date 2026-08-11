package common

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	MaxProcessLaunchItems          = 1024
	MaxProcessLaunchAggregateBytes = 1 << 20
	maxProcessPathBytes            = 8 << 10
)

// ProcessRequest is a complete, provider-neutral process specification.
type ProcessRequest struct {
	Executable       string
	Arguments        []string
	Environment      []string
	WorkingDirectory string
}

// Validate checks the bounded process-launch contract.
func (r ProcessRequest) Validate() error {
	if !validProcessPath(r.Executable) || !validProcessPath(r.WorkingDirectory) || r.Arguments == nil || r.Environment == nil {
		return errors.New("invalid provider launch request")
	}
	if len(r.Arguments) > MaxProcessLaunchItems || len(r.Environment) > MaxProcessLaunchItems {
		return errors.New("provider launch request exceeds item limit")
	}
	total := len(r.Executable) + len(r.WorkingDirectory)
	for _, argument := range r.Arguments {
		if !utf8.ValidString(argument) || strings.ContainsRune(argument, '\x00') {
			return errors.New("invalid provider launch argument")
		}
		total += len(argument)
	}
	seenEnvironment := make(map[string]struct{}, len(r.Environment))
	for _, entry := range r.Environment {
		separator := strings.IndexByte(entry, '=')
		if !utf8.ValidString(entry) || strings.ContainsRune(entry, '\x00') || separator <= 0 {
			return errors.New("invalid provider launch environment")
		}
		name := entry[:separator]
		if _, duplicate := seenEnvironment[name]; duplicate {
			return errors.New("duplicate provider launch environment variable")
		}
		seenEnvironment[name] = struct{}{}
		total += len(entry)
	}
	if total > MaxProcessLaunchAggregateBytes {
		return errors.New("provider launch request exceeds byte limit")
	}
	return nil
}

func validProcessPath(value string) bool {
	return value != "" && len(value) <= maxProcessPathBytes && utf8.ValidString(value) && !hasDisallowedProcessC0(value) && filepath.IsAbs(value) && filepath.Clean(value) == value
}

func hasDisallowedProcessC0(value string) bool {
	for _, character := range value {
		if character < 0x20 && character != '\t' && character != '\n' && character != '\r' {
			return true
		}
	}
	return false
}

// ProcessLauncher starts one isolated process from an explicit specification.
type ProcessLauncher interface {
	Launch(context.Context, ProcessRequest) (ManagedProcess, error)
}

// ManagedProcess owns process I/O and process-group escalation.
type ManagedProcess interface {
	Input() io.WriteCloser
	Output() io.Reader
	Errors() io.Reader
	Wait() error
	Terminate() error
	Kill() error
}

// ProcessErrorKind classifies process-launch and process-group failures.
type ProcessErrorKind string

const (
	ProcessErrorInvalidRequest     ProcessErrorKind = "invalid_request"
	ProcessErrorExecutable         ProcessErrorKind = "executable"
	ProcessErrorWorkingDirectory   ProcessErrorKind = "working_directory"
	ProcessErrorStart              ProcessErrorKind = "start"
	ProcessErrorCanceled           ProcessErrorKind = "canceled"
	ProcessErrorUnsupported        ProcessErrorKind = "unsupported_platform"
	ProcessErrorUnsafeProcessGroup ProcessErrorKind = "unsafe_process_group"
	ProcessErrorSignal             ProcessErrorKind = "signal"
)

// ProcessError is a classified process-group operation failure.
type ProcessError struct {
	Kind ProcessErrorKind
	Op   string
	Err  error
}

func (e *ProcessError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err == nil {
		return fmt.Sprintf("process group %s failed (%s)", e.Op, e.Kind)
	}
	return fmt.Sprintf("process group %s failed (%s): %v", e.Op, e.Kind, e.Err)
}

func (e *ProcessError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func processError(kind ProcessErrorKind, operation string, err error) error {
	if err == nil {
		err = errors.New("operation failed")
	}
	return &ProcessError{Kind: kind, Op: operation, Err: err}
}

// ProcessGroupLauncher starts children in isolated operating-system process groups.
// Its zero value is ready for use.
type ProcessGroupLauncher struct{}

// NewProcessGroupLauncher returns an isolated process launcher.
func NewProcessGroupLauncher() *ProcessGroupLauncher { return &ProcessGroupLauncher{} }

var _ ProcessLauncher = (*ProcessGroupLauncher)(nil)
