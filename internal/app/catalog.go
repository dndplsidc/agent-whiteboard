package app

import (
	"path/filepath"

	"github.com/dndplsidc/agent-whiteboard/internal/catalog"
	"github.com/dndplsidc/agent-whiteboard/internal/common"
	"github.com/dndplsidc/agent-whiteboard/internal/config"
)

// NewLocalCatalog constructs the client-owned catalog rooted in the effective
// user's home. It does not create the catalog until a publishing command runs.
func NewLocalCatalog() (*catalog.Store, error) {
	home, err := config.ResolvePath("~")
	if err != nil {
		return nil, err
	}
	return catalog.New(catalog.Config{
		Root:  filepath.Join(home, ".agent-whiteboard", "catalog"),
		Clock: common.SystemClock{},
	})
}
