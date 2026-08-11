//go:build unix

package pi

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

func TestBuildPromptFieldsReadsOrderedNativeImages(t *testing.T) {
	root := t.TempDir()
	imagesDirectory := filepath.Join(root, ".agent-images")
	require.NoError(t, os.Mkdir(imagesDirectory, 0o700))
	firstPath := filepath.Join(imagesDirectory, "first.png")
	secondPath := filepath.Join(imagesDirectory, "second.webp")
	require.NoError(t, os.WriteFile(firstPath, []byte("first"), 0o600))
	require.NoError(t, os.WriteFile(secondPath, []byte("second"), 0o600))
	images := []provider.ImageInput{
		{ID: strings.Repeat("A", 32), Name: "first.png", MediaType: "image/png", Bytes: 5, Path: firstPath},
		{ID: strings.Repeat("B", 32), Name: "second.webp", MediaType: "image/webp", Bytes: 6, Path: secondPath},
	}

	fields, err := buildPromptFields([]byte("envelope"), images)
	require.NoError(t, err)
	require.Equal(t, "envelope", fields["message"])
	require.Equal(t, []piImageContent{
		{Type: "image", Data: base64.StdEncoding.EncodeToString([]byte("first")), MIMEType: "image/png"},
		{Type: "image", Data: base64.StdEncoding.EncodeToString([]byte("second")), MIMEType: "image/webp"},
	}, fields["images"])
}

func TestBuildPromptFieldsRejectsSubstitutedOrMismatchedFiles(t *testing.T) {
	root := t.TempDir()
	imagesDirectory := filepath.Join(root, ".agent-images")
	require.NoError(t, os.Mkdir(imagesDirectory, 0o700))
	realPath := filepath.Join(imagesDirectory, "real.png")
	linkPath := filepath.Join(imagesDirectory, "link.png")
	require.NoError(t, os.WriteFile(realPath, []byte("image"), 0o600))
	require.NoError(t, os.Symlink(realPath, linkPath))

	for _, image := range []provider.ImageInput{
		{ID: strings.Repeat("A", 32), Name: "link.png", MediaType: "image/png", Bytes: 5, Path: linkPath},
		{ID: strings.Repeat("A", 32), Name: "real.png", MediaType: "image/png", Bytes: 4, Path: realPath},
	} {
		_, err := buildPromptFields([]byte("envelope"), []provider.ImageInput{image})
		require.Error(t, err)
	}
}

func TestPiModelImageCapabilityUsesDeclaredInputModalities(t *testing.T) {
	require.True(t, modelSupportsImages([]string{"text", "image"}))
	require.False(t, modelSupportsImages([]string{"text"}))
	require.False(t, modelSupportsImages(nil))
}

func TestSessionSubmitSendsNativePiImages(t *testing.T) {
	root := t.TempDir()
	imagesDirectory := filepath.Join(root, ".agent-images")
	require.NoError(t, os.Mkdir(imagesDirectory, 0o700))
	path := filepath.Join(imagesDirectory, "image.png")
	require.NoError(t, os.WriteFile(path, []byte("image"), 0o600))
	child := newRPCFakeChild()
	client, err := newRPCClient(child)
	require.NoError(t, err)
	driver := &Driver{config: Config{Clock: fixedClock{value: time.Unix(10, 0).UTC()}}}
	session := newSession(driver, provider.NativeSession{}, startupState{Model: "p/m", SupportsImages: true}, child, client)
	request := provider.TurnRequest{
		TurnID: strings.Repeat("A", 32), MessageID: strings.Repeat("B", 32), Content: provider.TextMessage("compare"),
		Images: []provider.ImageInput{{ID: strings.Repeat("C", 32), Name: "image.png", MediaType: "image/png", Bytes: 5, Path: path}},
	}
	result := make(chan error, 1)
	go func() { _, submitErr := session.Submit(context.Background(), request); result <- submitErr }()
	select {
	case submitErr := <-result:
		require.NoError(t, submitErr, "submit returned before writing the native prompt")
		t.Fatal("submit returned before writing the native prompt")
	case <-time.After(50 * time.Millisecond):
	}
	command := child.readCommand(t)
	require.Equal(t, "prompt", command["type"])
	images, ok := command["images"].([]any)
	require.True(t, ok)
	require.Len(t, images, 1)
	require.Equal(t, map[string]any{"type": "image", "data": base64.StdEncoding.EncodeToString([]byte("image")), "mimeType": "image/png"}, images[0])
	child.writeRecord(t, map[string]any{"id": command["id"], "type": "response", "command": "prompt", "success": true})
	require.NoError(t, <-result)
	child.closeOutput()
}
