package common_test

import (
	"errors"
	"testing"

	"github.com/dndplsidc/agent-whiteboard/internal/common"
	"github.com/stretchr/testify/require"
)

func TestErrorWrapAndCode(t *testing.T) {
	cause := errors.New("disk failed")
	err := common.NewError(common.CodeStorageUnavailable, "storage unavailable", cause)
	require.ErrorIs(t, err, cause)
	require.True(t, common.HasCode(err, common.CodeStorageUnavailable))
	require.Equal(t, "storage unavailable", err.Error())
}
