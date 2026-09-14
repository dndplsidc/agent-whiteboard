package broker

import (
	"testing"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/protocol"
	"github.com/stretchr/testify/require"
)

func TestReplayExclusionPreservesVisibilityAfterEviction(t *testing.T) {
	log := NewReplayLog()
	origin, observer := testID('D'), testID('E')
	event := protocol.Event{APIVersion: protocol.APIVersion, EventID: sequenceID(1), ConversationID: testID('C'), Type: protocol.EventError, Timestamp: testTime(), Payload: protocol.ErrorPayload{Error: protocol.NewBrowserError(protocol.ErrorContextTooLarge)}}
	require.Error(t, log.AppendExceptClient("", event))
	require.NoError(t, log.AppendExceptClient(origin, event))
	events, err := log.Replay(origin, "")
	require.NoError(t, err)
	require.Empty(t, events)
	for _, client := range []string{observer, testID('F')} {
		events, err = log.Replay(client, "")
		require.NoError(t, err)
		require.Equal(t, []protocol.Event{event}, events)
	}
	_, err = log.Replay(origin, event.EventID)
	require.ErrorIs(t, err, ErrReplayCursorMissing)
	_, err = log.Replay(observer, event.EventID)
	require.NoError(t, err)
	for i := 0; i < MaxReplayEvents; i++ {
		next := event
		next.EventID = sequenceID(uint64(i + 2))
		require.NoError(t, log.Append(next))
	}
	_, err = log.Replay(origin, event.EventID)
	require.ErrorIs(t, err, ErrReplayCursorMissing)
	_, err = log.Replay(observer, event.EventID)
	require.ErrorIs(t, err, ErrReplayCursorEvicted)
}
