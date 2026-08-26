package image

import (
	"github.com/dndplsidc/agent-whiteboard/internal/common"
)

func DetectFormat(content []byte) (string, string, error) {
	format, err := common.DetectRaster(content)
	if err != nil {
		return "", "", unsupportedMediaType()
	}
	return format.Extension, format.MediaType, nil
}

func unsupportedMediaType() error {
	return common.NewError(common.CodeUnsupportedMediaType, "unsupported image format", nil)
}
