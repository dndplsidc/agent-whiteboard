package common_test

import (
	"strings"
	"testing"

	"github.com/dndplsidc/agent-whiteboard/internal/common"
	"github.com/stretchr/testify/require"
)

func TestCryptoIDGenerator(t *testing.T) {
	id, err := (common.CryptoIDGenerator{}).NewID()
	require.NoError(t, err)
	require.Len(t, id, 32)
	require.NoError(t, common.ValidateID(id))
	require.NotContains(t, id, "=")
}

func TestValidateID(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{name: "too short", id: "short"},
		{name: "invalid encoding", id: strings.Repeat("!", 32)},
		{name: "padded encoding", id: strings.Repeat("A", 31) + "="},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := common.ValidateID(tt.id)
			require.Error(t, err)
			require.True(t, common.HasCode(err, common.CodeInvalidRequest))
		})
	}
}
