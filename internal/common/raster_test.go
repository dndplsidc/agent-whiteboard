package common_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/stretchr/testify/require"
)

func TestDetectReturnsNeutralRasterMetadata(t *testing.T) {
	value := image.NewRGBA(image.Rect(0, 0, 1, 1))
	value.Set(0, 0, color.RGBA{R: 1, A: 255})
	var content bytes.Buffer
	require.NoError(t, png.Encode(&content, value))

	format, err := common.DetectRaster(content.Bytes())
	require.NoError(t, err)
	require.Equal(t, common.RasterFormat{Extension: ".png", MediaType: "image/png"}, format)
}

func TestDetectRejectsUnsupportedAndMalformedInput(t *testing.T) {
	for _, content := range [][]byte{[]byte("not an image"), []byte("\x89PNG\r\n\x1a\n")} {
		_, err := common.DetectRaster(content)
		require.ErrorIs(t, err, common.ErrUnsupportedRaster)
	}
}
