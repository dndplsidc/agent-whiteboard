package broker

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/protocol"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
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

func TestRejectedSubmissionsShowOncePerClientAndKeepSeparateAttempts(t *testing.T) {
	for _, stage := range []string{"preflight", "submit"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				broker, _, session, origin, clientID, identity, resource, page := turnFixture(t, 9400)
				defer broker.Close(context.Background())
				session.mu.Lock()
				if stage == "preflight" {
					session.preflightErr = provider.NewProviderError(provider.ErrorContextTooLarge)
				} else {
					session.submitErr = provider.NewProviderError(provider.ErrorContextTooLarge)
				}
				session.mu.Unlock()
				connected, err := broker.Connect(context.Background(), identity.Origin, observationConnect(sequenceID(9402), identity.CapabilityID, page.Digest, resource, ""))
				require.NoError(t, err)
				observer := connected.(*Connection)
				defer observer.Close(context.Background())
				require.Equal(t, protocol.EventSnapshot, receiveLifecycle(t, observer.Events()).Type)
				for attempt := uint64(0); attempt < 2; attempt++ {
					result, err := origin.Command(context.Background(), submitCommand(sequenceID(9410+attempt*3), clientID, origin.ConversationID(), sequenceID(9411+attempt*3), sequenceID(9412+attempt*3), "question", &page))
					require.NoError(t, err)
					requireCommandResult(t, result, protocol.CommandRejected, protocol.ErrorContextTooLarge)
					require.Equal(t, protocol.EventLifecycle, receiveLifecycle(t, origin.Events()).Type)
					require.Equal(t, result, receiveLifecycle(t, origin.Events()))
					shared := receiveLifecycle(t, observer.Events())
					require.Equal(t, protocol.EventError, shared.Type)
					require.Equal(t, protocol.ErrorContextTooLarge, shared.Payload.(protocol.ErrorPayload).Error.Code())
					require.Equal(t, protocol.EventLifecycle, receiveLifecycle(t, observer.Events()).Type)
					synctest.Wait()
					require.Empty(t, origin.Events())
					require.Empty(t, observer.Events())
				}
			})
		})
	}
}
