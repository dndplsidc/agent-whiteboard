package protocol_test

import (
	"strings"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	"github.com/stretchr/testify/require"
)

func TestImageAttachmentLimitsAreFrozen(t *testing.T) {
	require.Equal(t, "5", protocol.APIVersion)
	require.Equal(t, "agent-whiteboard.v5", protocol.WebSocketSubprotocol)
	require.Equal(t, "/api/v1/agent/images", protocol.ImagesPath)
	require.Equal(t, 8, protocol.MaxImagesPerTurn)
	require.Equal(t, 10<<20, protocol.MaxImageBytes)
	require.Equal(t, 20<<20, protocol.MaxTurnImageBytes)
	require.Equal(t, int64(512<<20), protocol.MaxConversationImageBytes)
	require.Equal(t, 255, protocol.MaxImageNameBytes)
}

func TestSubmitImagesRoundTripAndPermitImageOnlyTurns(t *testing.T) {
	conversationID := idC
	command := protocol.Command{
		APIVersion: protocol.APIVersion, CommandID: idA, ClientID: idB,
		ConversationID: &conversationID, Type: protocol.CommandSubmit,
		Payload: protocol.SubmitPayload{
			TurnID: strings.Repeat("D", 32), MessageID: strings.Repeat("E", 32),
			Content: protocol.TextContent(""),
			Images: []protocol.ImageReference{
				{ImageID: strings.Repeat("F", 32), Name: "first.png"},
				{ImageID: strings.Repeat("G", 32), Name: "second.webp"},
			},
		},
	}

	encoded, err := protocol.EncodeCommand(command)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "image/png")
	require.NotContains(t, string(encoded), "/private/")

	decoded, err := protocol.DecodeCommand(encoded)
	require.NoError(t, err)
	require.Equal(t, command, decoded)
}

func TestSubmitImageReferencesRejectInvalidBoundaries(t *testing.T) {
	valid := protocol.ImageReference{ImageID: strings.Repeat("F", 32), Name: "screen.png"}
	tests := []struct {
		name    string
		message string
		images  []protocol.ImageReference
	}{
		{name: "empty turn"},
		{name: "too many", images: repeatImageReferences(protocol.MaxImagesPerTurn + 1)},
		{name: "duplicate id", images: []protocol.ImageReference{valid, valid}},
		{name: "bad id", images: []protocol.ImageReference{{ImageID: "bad", Name: "screen.png"}}},
		{name: "empty name", images: []protocol.ImageReference{{ImageID: valid.ImageID}}},
		{name: "long name", images: []protocol.ImageReference{{ImageID: valid.ImageID, Name: strings.Repeat("x", protocol.MaxImageNameBytes+1)}}},
		{name: "control name", images: []protocol.ImageReference{{ImageID: valid.ImageID, Name: "bad\x00.png"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conversationID := idC
			_, err := protocol.EncodeCommand(protocol.Command{
				APIVersion: protocol.APIVersion, CommandID: idA, ClientID: idB,
				ConversationID: &conversationID, Type: protocol.CommandSubmit,
				Payload: protocol.SubmitPayload{TurnID: strings.Repeat("D", 32), MessageID: strings.Repeat("E", 32), Content: protocol.TextContent(tt.message), Images: tt.images},
			})
			require.Error(t, err)
		})
	}
}

func TestImageDescriptorsSupportImageOnlyUserEvents(t *testing.T) {
	descriptors := []protocol.ImageDescriptor{{ImageID: strings.Repeat("F", 32), Name: "screen.png", MediaType: "image/png"}}
	empty := protocol.TextContent("")
	payloads := []protocol.EventPayload{
		protocol.UserMessagePayload{TurnID: strings.Repeat("D", 32), MessageID: strings.Repeat("E", 32), Content: empty, Images: descriptors, CreatedAt: time.Now().UTC()},
		protocol.QueuePayload{Items: []protocol.QueueItem{{TurnID: strings.Repeat("D", 32), MessageID: strings.Repeat("E", 32), Content: empty, Images: descriptors}}},
		protocol.TimelinePayload{CommandID: idA, Items: []protocol.TimelineItem{{ItemID: idB, Kind: protocol.TimelineUser, TurnID: strings.Repeat("D", 32), MessageID: strings.Repeat("E", 32), Content: &empty, Images: descriptors, CreatedAt: time.Now().UTC()}}},
	}
	for _, payload := range payloads {
		event := protocol.Event{APIVersion: protocol.APIVersion, EventID: idA, ConversationID: idC, Type: payload.EventType(), Timestamp: time.Now().UTC(), Payload: payload}
		encoded, err := protocol.EncodeEvent(event)
		require.NoError(t, err)
		require.Contains(t, string(encoded), `"images"`)
		decoded, err := protocol.DecodeEvent(encoded)
		require.NoError(t, err)
		require.Equal(t, event, decoded)
	}
}

func TestImageDescriptorsRejectUnsupportedMediaAndDuplicateIDs(t *testing.T) {
	now := time.Now().UTC()
	valid := protocol.ImageDescriptor{ImageID: strings.Repeat("F", 32), Name: "screen.png", MediaType: "image/png"}
	for _, descriptors := range [][]protocol.ImageDescriptor{
		{{ImageID: valid.ImageID, Name: valid.Name, MediaType: "image/svg+xml"}},
		{valid, valid},
	} {
		payload := protocol.UserMessagePayload{TurnID: strings.Repeat("D", 32), MessageID: strings.Repeat("E", 32), Images: descriptors, CreatedAt: now}
		_, err := protocol.EncodeEvent(protocol.Event{APIVersion: protocol.APIVersion, EventID: idA, ConversationID: idC, Type: payload.EventType(), Timestamp: now, Payload: payload})
		require.Error(t, err)
	}
}

func repeatImageReferences(count int) []protocol.ImageReference {
	result := make([]protocol.ImageReference, count)
	for index := range result {
		result[index] = protocol.ImageReference{ImageID: strings.Repeat(string(rune('F'+index)), 32), Name: "screen.png"}
	}
	return result
}
