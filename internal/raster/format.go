// Package raster validates the raster image formats accepted by Agent Whiteboard.
package raster

import (
	"bytes"
	"errors"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"

	"golang.org/x/image/webp"
)

// ErrUnsupported reports content that is not a decodable supported raster.
var ErrUnsupported = errors.New("unsupported raster image")

// Format is trusted metadata derived from image bytes, never from a filename
// or browser-supplied content type.
type Format struct {
	Extension string
	MediaType string
}

// Detect recognizes and config-decodes PNG, JPEG, GIF, and WebP content.
func Detect(content []byte) (Format, error) {
	mediaType := http.DetectContentType(content)
	reader := bytes.NewReader(content)

	var err error
	var extension string
	switch mediaType {
	case "image/png":
		_, err = png.DecodeConfig(reader)
		extension = ".png"
	case "image/jpeg":
		_, err = jpeg.DecodeConfig(reader)
		extension = ".jpg"
	case "image/gif":
		_, err = gif.DecodeConfig(reader)
		extension = ".gif"
	case "image/webp":
		_, err = webp.DecodeConfig(reader)
		extension = ".webp"
	default:
		return Format{}, ErrUnsupported
	}
	if err != nil {
		return Format{}, ErrUnsupported
	}
	return Format{Extension: extension, MediaType: mediaType}, nil
}

// SupportedMediaType reports whether mediaType is one of the trusted values
// Detect can produce.
func SupportedMediaType(mediaType string) bool {
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}
