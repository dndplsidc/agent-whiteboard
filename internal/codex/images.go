package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/edocsss/agent-whiteboard/internal/provider"
)

const maxModelPages = 32

type turnInput struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Path string `json:"path,omitempty"`
}

func buildTurnInput(workspace string, envelope []byte, images []provider.ImageInput) ([]turnInput, error) {
	input := make([]turnInput, 1, len(images)+1)
	input[0] = turnInput{Type: "text", Text: string(envelope)}
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

type modelPage struct {
	Data       []modelRecord `json:"data"`
	NextCursor *string       `json:"nextCursor"`
}

type modelRecord struct {
	ID              string          `json:"id"`
	Model           string          `json:"model"`
	InputModalities json.RawMessage `json:"inputModalities"`
}

func parseModelPage(raw []byte) (map[string]provider.Capabilities, string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var page modelPage
	if decoder.Decode(&page) != nil || decoder.Decode(&struct{}{}) != io.EOF || page.Data == nil {
		return nil, "", errors.New("invalid model/list response")
	}
	result := make(map[string]provider.Capabilities, len(page.Data)*2)
	for _, model := range page.Data {
		if model.ID == "" || model.Model == "" {
			return nil, "", errors.New("invalid model identity")
		}
		capabilities := provider.Capabilities{Images: true}
		if len(model.InputModalities) != 0 {
			if bytes.Equal(bytes.TrimSpace(model.InputModalities), []byte("null")) {
				return nil, "", errors.New("invalid model modalities")
			}
			var modalities []string
			if json.Unmarshal(model.InputModalities, &modalities) != nil || modalities == nil {
				return nil, "", errors.New("invalid model modalities")
			}
			capabilities.Images = false
			for _, modality := range modalities {
				switch modality {
				case "text", "audio":
				case "image":
					capabilities.Images = true
				default:
					return nil, "", errors.New("unknown model modality")
				}
			}
		}
		for _, key := range []string{model.ID, model.Model} {
			if existing, duplicate := result[key]; duplicate && existing != capabilities {
				return nil, "", errors.New("conflicting model capabilities")
			}
			result[key] = capabilities
		}
	}
	cursor := ""
	if page.NextCursor != nil {
		cursor = *page.NextCursor
		if cursor == "" {
			return nil, "", errors.New("invalid model cursor")
		}
	}
	return result, cursor, nil
}

func loadModelCapabilities(ctx context.Context, runtime *runtime) (map[string]provider.Capabilities, error) {
	models := make(map[string]provider.Capabilities)
	cursor := ""
	seenCursors := make(map[string]struct{})
	for page := 0; page < maxModelPages; page++ {
		params := map[string]any{"includeHidden": true}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := runtime.call(ctx, "model/list", params)
		if err != nil {
			return nil, err
		}
		pageModels, next, err := parseModelPage(raw)
		if err != nil {
			return nil, err
		}
		for key, capabilities := range pageModels {
			if existing, duplicate := models[key]; duplicate && existing != capabilities {
				return nil, errors.New("conflicting paged model capabilities")
			}
			models[key] = capabilities
		}
		if next == "" {
			return models, nil
		}
		if _, duplicate := seenCursors[next]; duplicate {
			return nil, errors.New("repeated model cursor")
		}
		seenCursors[next] = struct{}{}
		cursor = next
	}
	return nil, errors.New("model catalog pagination limit exceeded")
}
