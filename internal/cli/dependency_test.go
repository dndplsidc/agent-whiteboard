package cli

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCLIDoesNotImportPublicAgentWBFacade(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	for _, file := range files {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
		require.NoError(t, err)
		for _, imported := range parsed.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			require.NoError(t, err)
			require.NotEqual(t, "github.com/dndplsidc/agent-whiteboard/pkg/agentwb", path, file)
		}
	}
}
