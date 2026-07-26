//go:build !darwin && !linux

package config

import (
	"errors"
	"os"
)

var errTrustedOriginUnsupported = errors.New("trusted-origin editing is supported only on macOS and Linux")

type configDirectory struct{}

type fileIdentity struct {
	device uint64
	inode  uint64
}

type editLock struct{}

func openConfigDirectory(string) (*configDirectory, error) {
	return nil, errTrustedOriginUnsupported
}

func (directory *configDirectory) close() error { return nil }

func (directory *configDirectory) openTarget(string) (*os.File, error) {
	return nil, errTrustedOriginUnsupported
}

func (directory *configDirectory) targetIdentity(string) (fileIdentity, error) {
	return fileIdentity{}, errTrustedOriginUnsupported
}

func identityForFile(*os.File) (fileIdentity, error) {
	return fileIdentity{}, errTrustedOriginUnsupported
}

func (directory *configDirectory) createTemporary(string) (*os.File, string, error) {
	return nil, "", errTrustedOriginUnsupported
}

func (directory *configDirectory) rename(string, string) error { return errTrustedOriginUnsupported }
func (directory *configDirectory) unlink(string) error         { return errTrustedOriginUnsupported }
func (directory *configDirectory) sync() error                 { return errTrustedOriginUnsupported }

func acquireEditLock(*configDirectory, string) (*editLock, error) {
	return nil, errTrustedOriginUnsupported
}

func (lock *editLock) release() {}
