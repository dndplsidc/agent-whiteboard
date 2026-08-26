package codex

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
)

type turnInput struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Name string `json:"name,omitempty"`
	Path string `json:"path,omitempty"`
}

func buildTurnInput(workspace string, envelope []byte, skills []nativeSkill, images []provider.ImageInput) ([]turnInput, error) {
	input := make([]turnInput, 0, len(skills)+len(images)+1)
	for _, skill := range skills {
		if !validAbsolutePath(skill.path) || !validCatalogText(skill.name, provider.MaxSkillNameBytes, true) {
			return nil, errors.New("invalid Codex skill input")
		}
		input = append(input, turnInput{Type: "skill", Name: skill.name, Path: skill.path})
	}
	input = append(input, turnInput{Type: "text", Text: string(envelope)})
	if len(images) == 0 {
		return input, nil
	}
	if !filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace {
		return nil, errors.New("invalid Codex workspace")
	}
	for _, image := range images {
		if image.Validate() != nil || !pathWithin(workspace, image.Path) || verifyLocalImage(image) != nil {
			return nil, errors.New("unsafe Codex local image")
		}
		input = append(input, turnInput{Type: "localImage", Path: image.Path})
	}
	return input, nil
}

func pathWithin(workspace, path string) bool {
	relative, err := filepath.Rel(workspace, path)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func verifyLocalImage(image provider.ImageInput) error {
	before, err := os.Lstat(image.Path)
	if err != nil || !secureImageFile(before) || before.Size() != image.Bytes {
		return errors.New("unsafe local image")
	}
	parent, err := os.Lstat(filepath.Dir(image.Path))
	if err != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 || parent.Mode().Perm()&0o077 != 0 {
		return errors.New("unsafe local image directory")
	}
	file, err := os.Open(image.Path)
	if err != nil {
		return err
	}
	after, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil || closeErr != nil || !os.SameFile(before, after) {
		return errors.New("substituted local image")
	}
	return nil
}
