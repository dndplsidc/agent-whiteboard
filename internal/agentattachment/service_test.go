package agentattachment_test

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agentattachment"
	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/provider"
	"github.com/stretchr/testify/require"
)

const (
	conversationID = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	clientID       = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	turnID         = "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
	messageID      = "DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"
	origin         = "https://whiteboard.example"
)

func TestStageClaimReadAndReleaseLifecycle(t *testing.T) {
	service, workspace := newService(t)
	staged, err := service.Stage(t.Context(), agentattachment.StageRequest{
		Origin: origin, Provider: provider.NamePi, ConversationID: conversationID, ClientID: clientID,
		Content: bytes.NewReader(encodedPNG(t)),
	})
	require.NoError(t, err)
	require.Equal(t, "image/png", staged.MediaType)
	require.Positive(t, staged.Bytes)
	require.FileExists(t, filepath.Join(workspace, ".agent-images", staged.ImageID+".png"))

	preview, err := service.Read(t.Context(), agentattachment.ReadRequest{Origin: origin, ConversationID: conversationID, ClientID: clientID, ImageID: staged.ImageID})
	require.NoError(t, err)
	require.Equal(t, encodedPNG(t), preview.Content)

	claimed, err := service.Claim(t.Context(), agentattachment.ClaimRequest{
		Origin: origin, Provider: provider.NamePi, ConversationID: conversationID, ClientID: clientID,
		TurnID: turnID, MessageID: messageID,
		Images: []agentprotocol.ImageReference{{ImageID: staged.ImageID, Name: "pasted image.png"}},
	})
	require.NoError(t, err)
	require.Len(t, claimed.Inputs, 1)
	require.Equal(t, "pasted image.png", claimed.Inputs[0].Name)
	require.Equal(t, []agentprotocol.ImageDescriptor{{ImageID: staged.ImageID, Name: "pasted image.png", MediaType: "image/png"}}, claimed.Descriptors)
	require.NoError(t, claimed.Inputs[0].Validate())

	_, err = service.Claim(t.Context(), agentattachment.ClaimRequest{
		Origin: origin, Provider: provider.NamePi, ConversationID: conversationID, ClientID: clientID,
		TurnID: turnID, MessageID: messageID, Images: []agentprotocol.ImageReference{{ImageID: staged.ImageID, Name: "again.png"}},
	})
	require.ErrorIs(t, err, agentattachment.ErrMissing)

	descriptors, err := service.ImagesForMessage(t.Context(), conversationID, messageID)
	require.NoError(t, err)
	require.Equal(t, claimed.Descriptors, descriptors)
	require.NoError(t, service.ReleaseMessage(t.Context(), conversationID, messageID))
	require.NoFileExists(t, claimed.Inputs[0].Path)
}

func TestStageAndReadEnforceOwnershipAndValidation(t *testing.T) {
	service, _ := newService(t)
	_, err := service.Stage(t.Context(), agentattachment.StageRequest{
		Origin: origin, Provider: provider.NameCodex, ConversationID: conversationID, ClientID: clientID,
		Content: strings.NewReader("not an image"),
	})
	require.ErrorIs(t, err, agentattachment.ErrUnsupported)

	staged, err := service.Stage(t.Context(), agentattachment.StageRequest{
		Origin: origin, Provider: provider.NameCodex, ConversationID: conversationID, ClientID: clientID,
		Content: bytes.NewReader(encodedPNG(t)),
	})
	require.NoError(t, err)

	for _, request := range []agentattachment.ReadRequest{
		{Origin: "https://evil.example", ConversationID: conversationID, ClientID: clientID, ImageID: staged.ImageID},
		{Origin: origin, ConversationID: conversationID, ClientID: strings.Repeat("E", 32), ImageID: staged.ImageID},
		{Origin: origin, ConversationID: strings.Repeat("F", 32), ClientID: clientID, ImageID: staged.ImageID},
	} {
		_, err := service.Read(t.Context(), request)
		require.ErrorIs(t, err, agentattachment.ErrMissing)
	}

	err = service.DeleteStaged(t.Context(), agentattachment.DeleteRequest{Origin: "https://evil.example", ConversationID: conversationID, ClientID: clientID, ImageID: staged.ImageID})
	require.ErrorIs(t, err, agentattachment.ErrMissing)
	require.NoError(t, service.DeleteStaged(t.Context(), agentattachment.DeleteRequest{Origin: origin, ConversationID: conversationID, ClientID: clientID, ImageID: staged.ImageID}))
}

func TestStageEnforcesPerImageDraftAndConversationQuotas(t *testing.T) {
	service, _ := newService(t)
	_, err := service.Stage(t.Context(), agentattachment.StageRequest{
		Origin: origin, Provider: provider.NamePi, ConversationID: conversationID, ClientID: clientID,
		Content: io.LimitReader(infiniteReader{}, int64(agentprotocol.MaxImageBytes)+1),
	})
	require.ErrorIs(t, err, agentattachment.ErrImageTooLarge)

	for index := 0; index < agentprotocol.MaxImagesPerTurn; index++ {
		_, err := service.Stage(t.Context(), agentattachment.StageRequest{
			Origin: origin, Provider: provider.NamePi, ConversationID: conversationID, ClientID: clientID,
			Content: bytes.NewReader(encodedPNG(t)),
		})
		require.NoError(t, err)
	}
	_, err = service.Stage(t.Context(), agentattachment.StageRequest{
		Origin: origin, Provider: provider.NamePi, ConversationID: conversationID, ClientID: clientID,
		Content: bytes.NewReader(encodedPNG(t)),
	})
	require.ErrorIs(t, err, agentattachment.ErrTurnLimit)
}

func TestSweepExpiresOnlyUnclaimedImagesAndRepairsTemporaryFiles(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 8, 1, 0, 0, 0, time.UTC)}
	service, workspace := newServiceWithClock(t, clock)
	staged, err := service.Stage(t.Context(), agentattachment.StageRequest{Origin: origin, Provider: provider.NamePi, ConversationID: conversationID, ClientID: clientID, Content: bytes.NewReader(encodedPNG(t))})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(workspace, ".agent-images", "orphan.uploading"), []byte("partial"), 0o600))

	clock.Add(16 * time.Minute)
	require.NoError(t, service.Sweep(t.Context(), conversationID))
	require.NoFileExists(t, filepath.Join(workspace, ".agent-images", staged.ImageID+".png"))
	require.NoFileExists(t, filepath.Join(workspace, ".agent-images", "orphan.uploading"))
}

func TestSecurityRejectsSymlinkedAttachmentDirectory(t *testing.T) {
	service, workspace := newService(t)
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(workspace, ".agent-images")))
	_, err := service.Stage(t.Context(), agentattachment.StageRequest{Origin: origin, Provider: provider.NamePi, ConversationID: conversationID, ClientID: clientID, Content: bytes.NewReader(encodedPNG(t))})
	require.ErrorIs(t, err, agentattachment.ErrStorage)
	require.Empty(t, mustReadDir(t, outside))
}

type workspaces struct{ path string }

func (w workspaces) EnsureWorkspace(id string) (string, error) {
	if id != conversationID {
		return "", errors.New("unknown conversation")
	}
	return w.path, nil
}

type sequenceIDs struct {
	mu   sync.Mutex
	next byte
}

func (ids *sequenceIDs) NewID() (string, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	value := strings.Repeat(string(rune('E'+ids.next)), 32)
	ids.next++
	return value, nil
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *testClock) Add(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

func newService(t *testing.T) (*agentattachment.Service, string) {
	return newServiceWithClock(t, &testClock{now: time.Date(2026, 8, 8, 1, 0, 0, 0, time.UTC)})
}

func newServiceWithClock(t *testing.T, clock *testClock) (*agentattachment.Service, string) {
	t.Helper()
	workspace := filepath.Join(t.TempDir(), conversationID)
	require.NoError(t, os.Mkdir(workspace, 0o700))
	service, err := agentattachment.New(workspaces{path: workspace}, clock, &sequenceIDs{})
	require.NoError(t, err)
	return service, workspace
}

func encodedPNG(t *testing.T) []byte {
	t.Helper()
	value := image.NewRGBA(image.Rect(0, 0, 1, 1))
	value.Set(0, 0, color.RGBA{R: 10, G: 20, B: 30, A: 255})
	var output bytes.Buffer
	require.NoError(t, png.Encode(&output, value))
	return output.Bytes()
}

type infiniteReader struct{}

func (infiniteReader) Read(buffer []byte) (int, error) {
	for i := range buffer {
		buffer[i] = 'x'
	}
	return len(buffer), nil
}

func mustReadDir(t *testing.T, path string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(path)
	require.NoError(t, err)
	return entries
}
