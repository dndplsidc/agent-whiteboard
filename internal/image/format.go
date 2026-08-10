package image

import (
	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/raster"
)

func DetectFormat(content []byte) (string, string, error) {
	format, err := raster.Detect(content)
	if err != nil {
		return "", "", unsupportedMediaType()
	}
	return format.Extension, format.MediaType, nil
}

func unsupportedMediaType() error {
	return common.NewError(common.CodeUnsupportedMediaType, "unsupported image format", nil)
}
