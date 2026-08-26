//go:build unix

package pi

import (
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
)

type piImageContent struct {
	Type     string `json:"type"`
	Data     string `json:"data"`
	MIMEType string `json:"mimeType"`
}

func buildPromptFields(envelope []byte, images []provider.ImageInput) (map[string]any, error) {
	fields := map[string]any{"message": string(envelope)}
	if len(images) == 0 {
		return fields, nil
	}
	native := make([]piImageContent, len(images))
	for index, image := range images {
		content, err := readImageInput(image)
		if err != nil {
			return nil, err
		}
		native[index] = piImageContent{Type: "image", Data: base64.StdEncoding.EncodeToString(content), MIMEType: image.MediaType}
		wipe(content)
	}
	fields["images"] = native
	return fields, nil
}

func readImageInput(image provider.ImageInput) ([]byte, error) {
	if image.Validate() != nil {
		return nil, errors.New("invalid Pi image input")
	}
	before, err := os.Lstat(image.Path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0o077 != 0 || before.Size() != image.Bytes {
		return nil, errors.New("unsafe Pi image input")
	}
	parent, err := os.Lstat(filepath.Dir(image.Path))
	if err != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 || parent.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("unsafe Pi image directory")
	}
	if stat, ok := before.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 {
		return nil, errors.New("unsafe Pi image link count")
	}
	file, err := os.Open(image.Path)
	if err != nil {
		return nil, err
	}
	after, statErr := file.Stat()
	if statErr != nil || !os.SameFile(before, after) {
		file.Close()
		return nil, errors.New("substituted Pi image input")
	}
	content, readErr := io.ReadAll(io.LimitReader(file, image.Bytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || int64(len(content)) != image.Bytes {
		wipe(content)
		return nil, errors.New("read Pi image input")
	}
	return content, nil
}
