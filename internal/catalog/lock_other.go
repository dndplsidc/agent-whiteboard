//go:build !darwin && !linux

package catalog

import (
	"context"
	"errors"
	"os"
)

// The catalog is currently supported on the Unix platforms used by the CLI.
// Returning an error is safer than silently dropping cross-process ordering on
// a platform without the required lock implementation.
type fileLock struct{}

func openRecordFile(recordRoot, string) (*os.File, error) {
	return nil, errors.New("safe local catalog reads are unsupported on this platform")
}

func acquireFileLock(context.Context, *os.Root, string) (*fileLock, error) {
	return nil, errors.New("local catalog locking is unsupported on this platform")
}

func (*fileLock) release() {}

func syncDirectory(*os.Root) error { return nil }
