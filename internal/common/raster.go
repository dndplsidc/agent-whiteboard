package common

import (
	"bytes"
	"errors"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"

	"golang.org/x/image/webp"
)

// ErrUnsupportedRaster reports content that is not a decodable supported raster.
var ErrUnsupportedRaster = errors.New("unsupported raster image")

// RasterFormat is trusted metadata derived from image bytes, never from a filename
// or browser-supplied content type.
type RasterFormat struct {
	Extension string
	MediaType string
}

// DetectRaster recognizes and config-decodes PNG, JPEG, GIF, and WebP content.
func DetectRaster(content []byte) (RasterFormat, error) {
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
		return RasterFormat{}, ErrUnsupportedRaster
	}
	if err != nil {
		return RasterFormat{}, ErrUnsupportedRaster
	}
	return RasterFormat{Extension: extension, MediaType: mediaType}, nil
}

// SupportedRasterMediaType reports whether mediaType is one of the trusted
// values DetectRaster can produce.
func SupportedRasterMediaType(mediaType string) bool {
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}
