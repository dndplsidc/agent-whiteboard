package agentprotocol_test

import (
	"strings"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/stretchr/testify/require"
)

func TestImageAttachmentLimitsAreFrozen(t *testing.T) {
	require.Equal(t, "2", agentprotocol.APIVersion)
	require.Equal(t, "agent-whiteboard.v2", agentprotocol.WebSocketSubprotocol)
	require.Equal(t, "/api/v1/agent/images", agentprotocol.ImagesPath)
	require.Equal(t, 8, agentprotocol.MaxImagesPerTurn)
	require.Equal(t, 10<<20, agentprotocol.MaxImageBytes)
	require.Equal(t, 20<<20, agentprotocol.MaxTurnImageBytes)
	require.Equal(t, int64(512<<20), agentprotocol.MaxConversationImageBytes)
	require.Equal(t, 255, agentprotocol.MaxImageNameBytes)
}

func TestSubmitImagesRoundTripAndPermitImageOnlyTurns(t *testing.T) {
	conversationID := idC
	command := agentprotocol.Command{
		APIVersion: agentprotocol.APIVersion, CommandID: idA, ClientID: idB,
		ConversationID: &conversationID, Type: agentprotocol.CommandSubmit,
		Payload: agentprotocol.SubmitPayload{
			TurnID: strings.Repeat("D", 32), MessageID: strings.Repeat("E", 32),
			Images: []agentprotocol.ImageReference{
				{ImageID: strings.Repeat("F", 32), Name: "first.png"},
				{ImageID: strings.Repeat("G", 32), Name: "second.webp"},
			},
		},
	}

	encoded, err := agentprotocol.EncodeCommand(command)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "image/png")
	require.NotContains(t, string(encoded), "/private/")

	decoded, err := agentprotocol.DecodeCommand(encoded)
	require.NoError(t, err)
	require.Equal(t, command, decoded)
}

func TestSubmitImageReferencesRejectInvalidBoundaries(t *testing.T) {
	valid := agentprotocol.ImageReference{ImageID: strings.Repeat("F", 32), Name: "screen.png"}
	tests := []struct {
		name    string
		message string
		images  []agentprotocol.ImageReference
	}{
		{name: "empty turn"},
		{name: "too many", images: repeatImageReferences(agentprotocol.MaxImagesPerTurn + 1)},
		{name: "duplicate id", images: []agentprotocol.ImageReference{valid, valid}},
		{name: "bad id", images: []agentprotocol.ImageReference{{ImageID: "bad", Name: "screen.png"}}},
		{name: "empty name", images: []agentprotocol.ImageReference{{ImageID: valid.ImageID}}},
		{name: "long name", images: []agentprotocol.ImageReference{{ImageID: valid.ImageID, Name: strings.Repeat("x", agentprotocol.MaxImageNameBytes+1)}}},
		{name: "control name", images: []agentprotocol.ImageReference{{ImageID: valid.ImageID, Name: "bad\x00.png"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conversationID := idC
			_, err := agentprotocol.EncodeCommand(agentprotocol.Command{
				APIVersion: agentprotocol.APIVersion, CommandID: idA, ClientID: idB,
				ConversationID: &conversationID, Type: agentprotocol.CommandSubmit,
				Payload: agentprotocol.SubmitPayload{TurnID: strings.Repeat("D", 32), MessageID: strings.Repeat("E", 32), Message: tt.message, Images: tt.images},
			})
			require.Error(t, err)
		})
	}
}

func TestImageDescriptorsSupportImageOnlyUserEvents(t *testing.T) {
	descriptors := []agentprotocol.ImageDescriptor{{ImageID: strings.Repeat("F", 32), Name: "screen.png", MediaType: "image/png"}}
	payloads := []agentprotocol.EventPayload{
		agentprotocol.UserMessagePayload{TurnID: strings.Repeat("D", 32), MessageID: strings.Repeat("E", 32), Images: descriptors, CreatedAt: time.Now().UTC()},
		agentprotocol.QueuePayload{Items: []agentprotocol.QueueItem{{TurnID: strings.Repeat("D", 32), MessageID: strings.Repeat("E", 32), Images: descriptors}}},
		agentprotocol.TimelinePayload{CommandID: idA, Items: []agentprotocol.TimelineItem{{ItemID: idB, Kind: agentprotocol.TimelineUser, TurnID: strings.Repeat("D", 32), MessageID: strings.Repeat("E", 32), Images: descriptors, CreatedAt: time.Now().UTC()}}},
	}
	for _, payload := range payloads {
		event := agentprotocol.Event{APIVersion: agentprotocol.APIVersion, EventID: idA, ConversationID: idC, Type: payload.EventType(), Timestamp: time.Now().UTC(), Payload: payload}
		encoded, err := agentprotocol.EncodeEvent(event)
		require.NoError(t, err)
		require.Contains(t, string(encoded), `"images"`)
		decoded, err := agentprotocol.DecodeEvent(encoded)
		require.NoError(t, err)
		require.Equal(t, event, decoded)
	}
}

func TestImageDescriptorsRejectUnsupportedMediaAndDuplicateIDs(t *testing.T) {
	now := time.Now().UTC()
	valid := agentprotocol.ImageDescriptor{ImageID: strings.Repeat("F", 32), Name: "screen.png", MediaType: "image/png"}
	for _, descriptors := range [][]agentprotocol.ImageDescriptor{
		{{ImageID: valid.ImageID, Name: valid.Name, MediaType: "image/svg+xml"}},
		{valid, valid},
	} {
		payload := agentprotocol.UserMessagePayload{TurnID: strings.Repeat("D", 32), MessageID: strings.Repeat("E", 32), Images: descriptors, CreatedAt: now}
		_, err := agentprotocol.EncodeEvent(agentprotocol.Event{APIVersion: agentprotocol.APIVersion, EventID: idA, ConversationID: idC, Type: payload.EventType(), Timestamp: now, Payload: payload})
		require.Error(t, err)
	}
}

func repeatImageReferences(count int) []agentprotocol.ImageReference {
	result := make([]agentprotocol.ImageReference, count)
	for index := range result {
		result[index] = agentprotocol.ImageReference{ImageID: strings.Repeat(string(rune('F'+index)), 32), Name: "screen.png"}
	}
	return result
}
