//go:build unix

package pi

import (
	"encoding/json"
	"testing"

	"github.com/edocsss/agent-whiteboard/internal/provider"
	"github.com/stretchr/testify/require"
)

func TestAssistantMessageIDUsesStableDomainSeparatedDigest(t *testing.T) {
	const turnID = "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4"
	require.Equal(t, "stVSb4YOw9X2A6f7QhzOnTHe9i-MheoK", assistantMessageID(turnID))
	require.Len(t, assistantMessageID(turnID), 32)
}

func TestConservativeTokenBoundUsesOneTokenPerByte(t *testing.T) {
	require.Equal(t, 4, conservativeTokenBound(4))
	require.Equal(t, 5, conservativeTokenBound(5))
}

func TestDeriveBrokerItemsUsesEnvelopeIdentityAndDropsNativeIDs(t *testing.T) {
	request := provider.TurnRequest{TurnID: "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4", MessageID: "eHl6YWJjZGVmZ2hpamtsbW5vcHFyc3R1", Content: provider.TextMessage("reader text")}
	envelope, err := BuildEnvelope(request)
	require.NoError(t, err)
	message := func(role, text string) json.RawMessage {
		encoded, encodeErr := json.Marshal(map[string]any{"role": role, "content": []any{map[string]any{"type": "text", "text": text}}, "timestamp": "2026-01-02T03:04:05Z"})
		require.NoError(t, encodeErr)
		return encoded
	}
	items, ok := deriveBrokerItems([]nativeEntry{
		{ID: "/private/native-user", Type: "message", Message: message("user", string(envelope))},
		{ID: "secret-assistant-id", Type: "message", Message: message("assistant", "visible answer")},
	})
	require.True(t, ok)
	require.Len(t, items, 2)
	require.Equal(t, request.MessageID, items[0].item.MessageID)
	require.Equal(t, provider.TextMessage("reader text"), items[0].item.Content)
	require.Equal(t, assistantMessageID(request.TurnID), items[1].item.MessageID)
	require.Equal(t, "visible answer", items[1].item.Text)
}
