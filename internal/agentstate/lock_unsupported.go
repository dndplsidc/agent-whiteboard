//go:build !darwin && !linux

package agentstate

import (
	"os"
)

type secureDirectory struct{}
type stateLayout struct {
	rootPath      string
	state         *secureDirectory
	conversations *secureDirectory
	workspaces    *secureDirectory
	providers     *secureDirectory
}

func openStateLayout(string) (*stateLayout, error) { return nil, ErrUnsupported }
func (layout *stateLayout) close() error           { return nil }
func (directory *secureDirectory) ensureDirectory(string, bool) (*secureDirectory, bool, error) {
	return nil, false, ErrUnsupported
}
func (directory *secureDirectory) close() error { return nil }
func (directory *secureDirectory) createTemporary() (*os.File, string, error) {
	return nil, "", ErrUnsupported
}
func (directory *secureDirectory) readVerified(string, int64) ([]byte, fileIdentity, error) {
	return nil, fileIdentity{}, ErrUnsupported
}
func (directory *secureDirectory) targetIdentity(string) (fileIdentity, error) {
	return fileIdentity{}, ErrUnsupported
}
func (directory *secureDirectory) names() ([]string, error)     { return nil, ErrUnsupported }
func (directory *secureDirectory) rename(string, string) error  { return ErrUnsupported }
func (directory *secureDirectory) remove(string) error          { return ErrUnsupported }
func (directory *secureDirectory) removeDirectory(string) error { return ErrUnsupported }
func (directory *secureDirectory) sync() error                  { return ErrUnsupported }
