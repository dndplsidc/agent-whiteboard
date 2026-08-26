package image_test

import (
	"bytes"
	"encoding/base64"
	stdimage "image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/dndplsidc/agent-whiteboard/internal/common"
	imageDomain "github.com/dndplsidc/agent-whiteboard/internal/image"
	"github.com/stretchr/testify/require"
)

func TestDetectFormatAcceptsDecodableImages(t *testing.T) {
	tests := []struct {
		name      string
		content   []byte
		extension string
		mediaType string
	}{
		{name: "PNG", content: encodedPNG(t), extension: ".png", mediaType: "image/png"},
		{name: "JPEG", content: encodedJPEG(t), extension: ".jpg", mediaType: "image/jpeg"},
		{name: "GIF", content: encodedGIF(t), extension: ".gif", mediaType: "image/gif"},
		{name: "WebP", content: encodedWebP(t), extension: ".webp", mediaType: "image/webp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			extension, mediaType, err := imageDomain.DetectFormat(tt.content)
			require.NoError(t, err)
			require.Equal(t, tt.extension, extension)
			require.Equal(t, tt.mediaType, mediaType)
		})
	}
}

func TestDetectFormatRejectsUnsupportedSpoofedAndTruncatedContent(t *testing.T) {
	tests := []struct {
		name    string
		content []byte
	}{
		{name: "SVG", content: []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect width="1" height="1"/></svg>`)},
		{name: "random bytes", content: []byte{0xde, 0xad, 0xbe, 0xef}},
		{name: "truncated PNG", content: []byte("\x89PNG\r\n\x1a\n")},
		{name: "truncated JPEG", content: []byte("\xff\xd8\xff\xe0JFIF")},
		{name: "truncated GIF", content: []byte("GIF89a")},
		{name: "truncated WebP", content: []byte("RIFF\x10\x00\x00\x00WEBPVP8 ")},
		{name: "spoofed PNG", content: append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 504)...)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			extension, mediaType, err := imageDomain.DetectFormat(tt.content)
			require.Empty(t, extension)
			require.Empty(t, mediaType)
			require.True(t, common.HasCode(err, common.CodeUnsupportedMediaType))
			require.EqualError(t, err, "unsupported image format")
		})
	}
}

func encodedPNG(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	require.NoError(t, png.Encode(&output, onePixel()))
	return output.Bytes()
}

func encodedJPEG(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	require.NoError(t, jpeg.Encode(&output, onePixel(), nil))
	return output.Bytes()
}

func encodedGIF(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	require.NoError(t, gif.Encode(&output, onePixel(), nil))
	return output.Bytes()
}

func encodedWebP(t *testing.T) []byte {
	t.Helper()
	content, err := base64.StdEncoding.DecodeString("UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA")
	require.NoError(t, err)
	return content
}

func onePixel() stdimage.Image {
	image := stdimage.NewRGBA(stdimage.Rect(0, 0, 1, 1))
	image.Set(0, 0, color.RGBA{R: 25, G: 50, B: 75, A: 255})
	return image
}
