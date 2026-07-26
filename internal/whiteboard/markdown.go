package whiteboard

import (
	"unicode/utf8"

	"github.com/edocsss/agent-whiteboard/internal/common"
)

func validateMarkdown(source []byte) error {
	if len(source) == 0 {
		return common.NewError(common.CodeInvalidRequest, "markdown must not be empty", nil)
	}
	if !utf8.Valid(source) {
		return common.NewError(common.CodeInvalidRequest, "markdown must be UTF-8", nil)
	}
	return nil
}

func validateMarkdownContext(creatorContext []byte) error {
	if len(creatorContext) == 0 {
		return common.NewError(common.CodeInvalidRequest, "markdown context must not be empty", nil)
	}
	if !utf8.Valid(creatorContext) {
		return common.NewError(common.CodeInvalidRequest, "markdown context must be UTF-8", nil)
	}
	return nil
}
