//go:build !darwin && !linux

package config

import (
	"errors"
	"os"
)

type editLock struct{}

func acquireEditLock(string) (*editLock, error) {
	return nil, errors.New("trusted-origin editing is supported only on macOS and Linux")
}

func (lock *editLock) release() {}

func openRegularNoFollow(string) (*os.File, error) {
	return nil, errors.New("trusted-origin configuration is supported only on macOS and Linux")
}
