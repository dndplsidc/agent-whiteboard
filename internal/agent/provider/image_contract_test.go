package provider_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

func TestTurnRequestValidatesImageOnlyAndMixedInput(t *testing.T) {
	root := t.TempDir()
	image := provider.ImageInput{ID: strings.Repeat("A", 32), Name: "screen.png", MediaType: "image/png", Bytes: 1024, Path: filepath.Join(root, "screen.png")}
	for _, request := range []provider.TurnRequest{
		{TurnID: strings.Repeat("B", 32), MessageID: strings.Repeat("C", 32), Images: []provider.ImageInput{image}},
		{TurnID: strings.Repeat("B", 32), MessageID: strings.Repeat("C", 32), Content: provider.TextMessage("compare"), Images: []provider.ImageInput{image}},
	} {
		require.NoError(t, request.Validate())
	}
}

func TestTurnRequestRejectsInvalidImageBoundaries(t *testing.T) {
	root := t.TempDir()
	valid := provider.ImageInput{ID: strings.Repeat("A", 32), Name: "screen.png", MediaType: "image/png", Bytes: 1, Path: filepath.Join(root, "screen.png")}
	tests := []struct {
		name    string
		message string
		images  []provider.ImageInput
	}{
		{name: "empty turn"},
		{name: "too many", images: repeatProviderImages(valid, provider.MaxImagesPerTurn+1)},
		{name: "duplicate id", images: []provider.ImageInput{valid, valid}},
		{name: "duplicate path", images: []provider.ImageInput{valid, {ID: strings.Repeat("D", 32), Name: "other.png", MediaType: "image/png", Bytes: 1, Path: valid.Path}}},
		{name: "per image bytes", images: []provider.ImageInput{{ID: valid.ID, Name: valid.Name, MediaType: valid.MediaType, Bytes: provider.MaxImageBytes + 1, Path: valid.Path}}},
		{name: "aggregate bytes", images: []provider.ImageInput{
			{ID: strings.Repeat("A", 32), Name: "one.png", MediaType: "image/png", Bytes: provider.MaxImageBytes, Path: filepath.Join(root, "one.png")},
			{ID: strings.Repeat("D", 32), Name: "two.png", MediaType: "image/png", Bytes: provider.MaxImageBytes, Path: filepath.Join(root, "two.png")},
			{ID: strings.Repeat("E", 32), Name: "three.png", MediaType: "image/png", Bytes: 1, Path: filepath.Join(root, "three.png")},
		}},
		{name: "unsupported type", images: []provider.ImageInput{{ID: valid.ID, Name: valid.Name, MediaType: "image/svg+xml", Bytes: 1, Path: valid.Path}}},
		{name: "relative path", images: []provider.ImageInput{{ID: valid.ID, Name: valid.Name, MediaType: valid.MediaType, Bytes: 1, Path: "screen.png"}}},
		{name: "bad name", images: []provider.ImageInput{{ID: valid.ID, Name: "", MediaType: valid.MediaType, Bytes: 1, Path: valid.Path}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Error(t, (provider.TurnRequest{TurnID: strings.Repeat("B", 32), MessageID: strings.Repeat("C", 32), Content: provider.TextMessage(tt.message), Images: tt.images}).Validate())
		})
	}
}

func TestProviderImageCapabilitiesAndErrorsAreClosed(t *testing.T) {
	require.NoError(t, (provider.Capabilities{Images: true}).Validate())
	for _, code := range []provider.ProviderErrorCode{
		provider.ErrorImageInputUnsupported,
		provider.ErrorImageUnsupported,
		provider.ErrorImageTooLarge,
		provider.ErrorImageTurnLimit,
		provider.ErrorImageMissing,
		provider.ErrorImageStorageFailure,
	} {
		require.True(t, provider.NewProviderError(code).Valid(), code)
		require.Contains(t, provider.AllProviderErrorCodes(), code)
	}
}

func repeatProviderImages(template provider.ImageInput, count int) []provider.ImageInput {
	result := make([]provider.ImageInput, count)
	for index := range result {
		result[index] = template
		result[index].ID = strings.Repeat(string(rune('A'+index)), 32)
		result[index].Path = filepath.Join(filepath.Dir(template.Path), result[index].ID+".png")
	}
	return result
}
