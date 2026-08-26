package agentwb

import "github.com/dndplsidc/agent-whiteboard/internal/common"

type Error = common.Error
type ErrorCode = common.ErrorCode

var ErrIDCollision = common.ErrIDCollision

const (
	CodeInvalidRequest       = common.CodeInvalidRequest
	CodeNotFound             = common.CodeNotFound
	CodeContentTooLarge      = common.CodeContentTooLarge
	CodeUnsupportedMediaType = common.CodeUnsupportedMediaType
	CodeStorageUnavailable   = common.CodeStorageUnavailable
	CodeInternal             = common.CodeInternal
)

func HasErrorCode(err error, code ErrorCode) bool {
	return common.HasCode(err, code)
}
