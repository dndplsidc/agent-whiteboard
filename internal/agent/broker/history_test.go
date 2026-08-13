package broker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

func messageContentPointer(content protocol.MessageContent) *protocol.MessageContent {
	return &content
}

func TestCurrentHistoryPagesProviderMessagesWithStableBrowserIdentity(t *testing.T) {
	broker, _, session, connection, clientID, _, _, _ := turnFixture(t, 9000)
	defer broker.Close(context.Background())
	userMessageID := sequenceID(9001)
	assistantMessageID := sequenceID(9002)
	turnID := sequenceID(9003)
	session.mu.Lock()
	session.historyPage = provider.HistoryPage{
		Items: []provider.HistoryItem{
			{TurnID: turnID, MessageID: userMessageID, Role: provider.HistoryUser, Content: provider.TextMessage("question"), CreatedAt: testTime()},
			{TurnID: turnID, MessageID: assistantMessageID, Role: provider.HistoryAssistant, Text: "answer", CreatedAt: testTime().Add(lifecycleTestTimeout)},
		},
		NextCursor: assistantMessageID,
	}
	session.mu.Unlock()
	conversationID := connection.ConversationID()
	command := historyCommand(sequenceID(9004), clientID, conversationID, protocol.PageRequestPayload{Limit: 2})

	result, err := connection.Command(context.Background(), command)
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandSucceeded, "")
	timeline := receiveLifecycle(t, connection.Events())
	require.Equal(t, protocol.EventTimeline, timeline.Type)
	payload := timeline.Payload.(protocol.TimelinePayload)
	require.Equal(t, command.CommandID, payload.CommandID)
	require.Equal(t, []protocol.TimelineItem{
		{ItemID: userMessageID, Kind: protocol.TimelineUser, TurnID: turnID, MessageID: userMessageID, Content: messageContentPointer(protocol.TextContent("question")), CreatedAt: testTime()},
		{ItemID: assistantMessageID, Kind: protocol.TimelineAssistant, TurnID: turnID, MessageID: assistantMessageID, Text: "answer", CreatedAt: testTime().Add(lifecycleTestTimeout)},
	}, payload.Items)
	require.NotNil(t, payload.NextCursor)
	require.Equal(t, assistantMessageID, *payload.NextCursor)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))

	duplicate, err := connection.Command(context.Background(), command)
	require.NoError(t, err)
	require.Equal(t, result, duplicate)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))
}

func TestCurrentHistoryTimelineIsIsolatedFromOtherClientsLiveAndInReplay(t *testing.T) {
	broker, _, session, first, firstClientID, identity, resource, page := turnFixture(t, 9005)
	defer broker.Close(context.Background())
	secondClientID := sequenceID(9050)
	secondRaw, err := broker.Connect(context.Background(), identity.Origin, observationConnect(secondClientID, identity.CapabilityID, page.Digest, resource, ""))
	require.NoError(t, err)
	second := secondRaw.(*Connection)
	defer second.Close(context.Background())
	secondCheckpoint := receiveLifecycle(t, second.Events())
	turnID := sequenceID(9007)
	messageID := sequenceID(9008)
	session.mu.Lock()
	session.historyPage = provider.HistoryPage{Items: []provider.HistoryItem{{TurnID: turnID, MessageID: messageID, Role: provider.HistoryUser, Content: provider.TextMessage("private timeline"), CreatedAt: testTime()}}}
	session.mu.Unlock()
	conversationID := first.ConversationID()
	command := historyCommand(sequenceID(9009), firstClientID, conversationID, protocol.PageRequestPayload{})
	result, err := first.Command(context.Background(), command)
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandSucceeded, "")
	require.Equal(t, protocol.EventTimeline, receiveLifecycle(t, first.Events()).Type)
	require.Equal(t, result, receiveLifecycle(t, first.Events()))

	resync := protocol.Command{APIVersion: protocol.APIVersion, CommandID: sequenceID(9010), ClientID: secondClientID, ConversationID: &conversationID, Type: protocol.CommandResync, Payload: protocol.ResyncPayload{AfterEventID: secondCheckpoint.EventID}}
	resyncResult, err := second.Command(context.Background(), resync)
	require.NoError(t, err)
	requireCommandResult(t, resyncResult, protocol.CommandSucceeded, "")
	require.Equal(t, protocol.EventSnapshot, receiveLifecycle(t, second.Events()).Type, "another client's targeted timeline must be absent from live delivery and replay")
	require.Equal(t, resyncResult, receiveLifecycle(t, second.Events()))
}

func TestCurrentHistoryRedactsProviderFailureAndRejectsMalformedPage(t *testing.T) {
	broker, _, session, connection, clientID, _, _, _ := turnFixture(t, 9010)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()

	session.mu.Lock()
	session.historyErr = errors.New("provider /private/native/history")
	session.mu.Unlock()
	failed := historyCommand(sequenceID(9011), clientID, conversationID, protocol.PageRequestPayload{})
	result, err := connection.Command(context.Background(), failed)
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandRejected, protocol.ErrorProviderProtocolFailure)
	require.NotContains(t, result.Payload.(protocol.CommandResultPayload).Error.Message(), "/private")
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))

	session.mu.Lock()
	session.historyErr = nil
	session.historyPage = provider.HistoryPage{Items: []provider.HistoryItem{{TurnID: sequenceID(9012), MessageID: sequenceID(9013), Role: provider.HistoryRole("native"), Text: "hidden", CreatedAt: testTime()}}}
	session.mu.Unlock()
	malformed := historyCommand(sequenceID(9014), clientID, conversationID, protocol.PageRequestPayload{})
	result, err = connection.Command(context.Background(), malformed)
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandRejected, protocol.ErrorProviderProtocolFailure)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))
}

func TestCurrentHistoryRejectsProviderPagesBeyondRequestedOrBrowserBoundsIdempotently(t *testing.T) {
	for _, test := range []struct {
		name  string
		limit int
		items []provider.HistoryItem
	}{
		{
			name:  "requested limit",
			limit: 1,
			items: []provider.HistoryItem{
				{TurnID: sequenceID(9015), MessageID: sequenceID(9016), Role: provider.HistoryUser, Content: provider.TextMessage("one"), CreatedAt: testTime()},
				{TurnID: sequenceID(9015), MessageID: sequenceID(9017), Role: provider.HistoryAssistant, Text: "two", CreatedAt: testTime()},
			},
		},
		{
			name:  "browser byte bound",
			limit: 2,
			items: []provider.HistoryItem{
				{TurnID: sequenceID(9018), MessageID: sequenceID(9019), Role: provider.HistoryUser, Content: provider.TextMessage(strings.Repeat("x", protocol.MaxMessageBytes)), CreatedAt: testTime()},
				{TurnID: sequenceID(9018), MessageID: sequenceID(9020), Role: provider.HistoryAssistant, Text: strings.Repeat("y", protocol.MaxMessageBytes), CreatedAt: testTime()},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			broker, _, session, connection, clientID, _, _, _ := turnFixture(t, 9040)
			defer broker.Close(context.Background())
			session.mu.Lock()
			session.historyPage = provider.HistoryPage{Items: test.items}
			session.mu.Unlock()
			conversationID := connection.ConversationID()
			command := historyCommand(sequenceID(9041), clientID, conversationID, protocol.PageRequestPayload{Limit: test.limit})

			result, err := connection.Command(context.Background(), command)
			require.NoError(t, err)
			requireCommandResult(t, result, protocol.CommandRejected, protocol.ErrorProviderProtocolFailure)
			require.Equal(t, result, receiveLifecycle(t, connection.Events()))
			duplicate, err := connection.Command(context.Background(), command)
			require.NoError(t, err)
			require.Equal(t, result, duplicate)
			require.Equal(t, result, receiveLifecycle(t, connection.Events()))
			require.EqualValues(t, 1, session.historyCalls.Load())
		})
	}
}

func TestHistoryWorkerDelaysQueuedDispatchWithoutBlockingProviderEvents(t *testing.T) {
	broker, _, session, connection, clientID, _, _, page := turnFixture(t, 9020)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	activeTurnID := sequenceID(9021)
	active := submitCommand(sequenceID(9022), clientID, conversationID, activeTurnID, sequenceID(9023), "active", &page)
	_, err := connection.Command(context.Background(), active)
	require.NoError(t, err)
	receiveLifecycle(t, session.submitted)
	drainEvents(t, connection.Events(), 3)
	queuedTurnID := sequenceID(9024)
	queued := submitCommand(sequenceID(9025), clientID, conversationID, queuedTurnID, sequenceID(9026), "queued", nil)
	_, err = connection.Command(context.Background(), queued)
	require.NoError(t, err)
	drainEvents(t, connection.Events(), 2)

	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	session.mu.Lock()
	session.historyPage = provider.HistoryPage{Items: []provider.HistoryItem{}}
	session.historyEntered = entered
	session.historyGate = gate
	session.mu.Unlock()
	history := historyCommand(sequenceID(9027), clientID, conversationID, protocol.PageRequestPayload{})
	historyDone := make(chan commandResponse, 1)
	go func() {
		event, err := connection.Command(context.Background(), history)
		historyDone <- commandResponse{event: event, err: err}
	}()
	receiveLifecycle(t, entered)

	session.events <- provider.NewCompletionEvent(activeTurnID)
	require.Equal(t, protocol.EventCompletion, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, protocol.LifecycleReady, receiveLifecycle(t, connection.Events()).Payload.(protocol.LifecyclePayload).State)
	close(gate)
	response := receiveLifecycle(t, historyDone)
	require.NoError(t, response.err)
	requireCommandResult(t, response.event, protocol.CommandSucceeded, "")
	require.Equal(t, protocol.EventTimeline, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, response.event, receiveLifecycle(t, connection.Events()))
	queue := receiveLifecycle(t, connection.Events())
	require.Equal(t, protocol.EventQueue, queue.Type)
	require.Empty(t, queue.Payload.(protocol.QueuePayload).Items)
	require.Equal(t, protocol.LifecycleConnecting, receiveLifecycle(t, connection.Events()).Payload.(protocol.LifecyclePayload).State)
	require.Equal(t, queuedTurnID, receiveLifecycle(t, session.submitted).TurnID)
}

func TestInterruptWaitsForHistoryWorkerWithoutOverlappingProviderCalls(t *testing.T) {
	broker, _, session, connection, clientID, identity, resource, page := turnFixture(t, 9030)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	turnID := sequenceID(9031)
	active := submitCommand(sequenceID(9032), clientID, conversationID, turnID, sequenceID(9033), "active", &page)
	_, err := connection.Command(context.Background(), active)
	require.NoError(t, err)
	receiveLifecycle(t, session.submitted)
	drainEvents(t, connection.Events(), 3)

	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	session.mu.Lock()
	session.historyPage = provider.HistoryPage{Items: []provider.HistoryItem{}}
	session.historyEntered = entered
	session.historyGate = gate
	session.mu.Unlock()
	history := historyCommand(sequenceID(9034), clientID, conversationID, protocol.PageRequestPayload{})
	historyDone := make(chan commandResponse, 1)
	go func() {
		event, err := connection.Command(context.Background(), history)
		historyDone <- commandResponse{event: event, err: err}
	}()
	receiveLifecycle(t, entered)
	interrupt := protocol.Command{APIVersion: protocol.APIVersion, CommandID: sequenceID(9035), ClientID: clientID, ConversationID: &conversationID, Type: protocol.CommandInterrupt, Payload: protocol.WorkReferencePayload{WorkID: turnID}}
	interruptRequest := commandRequest{ctx: context.Background(), attachment: connection.attachment, command: interrupt, response: make(chan commandResponse, 1)}
	interruptReceived := make(chan struct{})
	go func() {
		connection.actor.requests <- interruptRequest
		close(interruptReceived)
	}()
	receiveLifecycle(t, interruptReceived)
	// A later attach cannot complete until the actor has returned from handling
	// the interrupt request, proving it was deferred while History stayed gated.
	barrierRaw, err := broker.Connect(context.Background(), identity.Origin, observationConnect(sequenceID(9036), identity.CapabilityID, page.Digest, resource, ""))
	require.NoError(t, err)
	barrier := barrierRaw.(*Connection)
	receiveLifecycle(t, barrier.Events())
	require.NoError(t, barrier.Close(context.Background()))

	close(gate)
	historyResponse := receiveLifecycle(t, historyDone)
	require.NoError(t, historyResponse.err)
	requireCommandResult(t, historyResponse.event, protocol.CommandSucceeded, "")
	require.Equal(t, turnID, receiveLifecycle(t, session.interrupted).TurnID)
	interruptResponse := receiveLifecycle(t, interruptRequest.response)
	require.NoError(t, interruptResponse.err)
	requireCommandResult(t, interruptResponse.event, protocol.CommandSucceeded, "")
	require.EqualValues(t, 1, session.maximumProviderCalls.Load(), "history and interrupt must use one provider worker lane")
}

func historyCommand(commandID, clientID, conversationID string, payload protocol.PageRequestPayload) protocol.Command {
	return protocol.Command{APIVersion: protocol.APIVersion, CommandID: commandID, ClientID: clientID, ConversationID: &conversationID, Type: protocol.CommandHistoryPage, Payload: payload}
}
