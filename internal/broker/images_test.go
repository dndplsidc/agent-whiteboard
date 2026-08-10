package broker

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agentattachment"
	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/agentstate"
	"github.com/edocsss/agent-whiteboard/internal/provider"
	"github.com/stretchr/testify/require"
)

type brokerAttachmentStore struct {
	mu          sync.Mutex
	claims      []agentattachment.ClaimRequest
	released    []string
	descriptors map[string][]agentprotocol.ImageDescriptor
	claimErr    error
	releaseErr  error
	sweepErr    error
	swept       []string
	removed     []string
}

func newBrokerAttachmentStore() *brokerAttachmentStore {
	return &brokerAttachmentStore{descriptors: make(map[string][]agentprotocol.ImageDescriptor)}
}

func (store *brokerAttachmentStore) Claim(_ context.Context, request agentattachment.ClaimRequest) (agentattachment.Claimed, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.claims = append(store.claims, request)
	if store.claimErr != nil {
		return agentattachment.Claimed{}, store.claimErr
	}
	claimed := agentattachment.Claimed{
		Inputs:      make([]provider.ImageInput, len(request.Images)),
		Descriptors: make([]agentprotocol.ImageDescriptor, len(request.Images)),
	}
	for index, image := range request.Images {
		claimed.Inputs[index] = provider.ImageInput{ID: image.ImageID, Name: image.Name, MediaType: "image/png", Bytes: 4, Path: filepath.Join("/private/tmp", image.ImageID+".png")}
		claimed.Descriptors[index] = agentprotocol.ImageDescriptor{ImageID: image.ImageID, Name: image.Name, MediaType: "image/png"}
	}
	store.descriptors[request.MessageID] = append([]agentprotocol.ImageDescriptor(nil), claimed.Descriptors...)
	return claimed, nil
}

func (store *brokerAttachmentStore) ImagesForMessage(_ context.Context, _ string, messageID string) ([]agentprotocol.ImageDescriptor, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]agentprotocol.ImageDescriptor(nil), store.descriptors[messageID]...), nil
}

func (store *brokerAttachmentStore) ReleaseMessage(_ context.Context, _ string, messageID string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.released = append(store.released, messageID)
	if store.releaseErr != nil {
		return store.releaseErr
	}
	delete(store.descriptors, messageID)
	return nil
}

func (store *brokerAttachmentStore) Sweep(_ context.Context, conversationID string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.swept = append(store.swept, conversationID)
	return store.sweepErr
}

func (store *brokerAttachmentStore) RemoveWorkspace(_ context.Context, conversationID string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.removed = append(store.removed, conversationID)
	return nil
}

func TestImageSubmitClaimsOrderedInputsAndDecoratesUserEventsAndHistory(t *testing.T) {
	store := newBrokerAttachmentStore()
	broker, _, session, connection, clientID, identity, resource, page := turnImageFixture(t, 8000, store, true)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	store.mu.Lock()
	require.Equal(t, []string{conversationID}, store.swept)
	store.mu.Unlock()
	turnID := sequenceID(8002)
	messageID := sequenceID(8003)
	references := []agentprotocol.ImageReference{
		{ImageID: sequenceID(8004), Name: "diagram.png"},
		{ImageID: sequenceID(8005), Name: "photo.png"},
	}
	command := submitCommand(sequenceID(8006), clientID, conversationID, turnID, messageID, "", &page)
	payload := command.Payload.(agentprotocol.SubmitPayload)
	payload.Images = references
	command.Payload = payload

	result, err := connection.Command(context.Background(), command)
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
	submitted := receiveLifecycle(t, session.submitted)
	require.True(t, submitted.Content.Empty())
	require.Equal(t, []string{references[0].ImageID, references[1].ImageID}, []string{submitted.Images[0].ID, submitted.Images[1].ID})
	drainEvents(t, connection.Events(), 3)

	store.mu.Lock()
	require.Equal(t, identity.Origin, store.claims[0].Origin)
	require.Equal(t, identity.Provider, store.claims[0].Provider)
	require.Equal(t, clientID, store.claims[0].ClientID)
	require.Equal(t, references, store.claims[0].Images)
	store.mu.Unlock()

	session.events <- provider.NewUserMessageEvent(turnID, messageID, provider.MessageContent{}, testTime())
	user := receiveLifecycle(t, connection.Events())
	require.Equal(t, agentprotocol.EventUserMessage, user.Type)
	require.Equal(t, []agentprotocol.ImageDescriptor{
		{ImageID: references[0].ImageID, Name: "diagram.png", MediaType: "image/png"},
		{ImageID: references[1].ImageID, Name: "photo.png", MediaType: "image/png"},
	}, user.Payload.(agentprotocol.UserMessagePayload).Images)

	session.events <- provider.NewCompletionEvent(turnID)
	require.Equal(t, agentprotocol.EventCompletion, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, agentprotocol.EventLifecycle, receiveLifecycle(t, connection.Events()).Type)
	session.mu.Lock()
	session.historyPage = provider.HistoryPage{Items: []provider.HistoryItem{{TurnID: turnID, MessageID: messageID, Role: provider.HistoryUser, Text: "", CreatedAt: testTime()}}}
	session.mu.Unlock()
	history := agentprotocol.Command{APIVersion: agentprotocol.APIVersion, CommandID: sequenceID(8007), ClientID: clientID, ConversationID: &conversationID, Type: agentprotocol.CommandHistoryPage, Payload: agentprotocol.PageRequestPayload{Limit: 50}}
	historyResult, err := connection.Command(context.Background(), history)
	require.NoError(t, err)
	requireCommandResult(t, historyResult, agentprotocol.CommandSucceeded, "")
	timeline := receiveLifecycle(t, connection.Events())
	require.Equal(t, agentprotocol.EventTimeline, timeline.Type)
	require.Equal(t, user.Payload.(agentprotocol.UserMessagePayload).Images, timeline.Payload.(agentprotocol.TimelinePayload).Items[0].Images)
	require.Equal(t, historyResult, receiveLifecycle(t, connection.Events()))
	_ = resource
}

func TestConversationPeriodicallySweepsStagedImages(t *testing.T) {
	store := newBrokerAttachmentStore()
	timers := newManualTimerFactory()
	broker, _, _, connection, _, _, _, _ := turnImageFixtureWithTimers(t, 8050, store, true, timers)
	defer broker.Close(context.Background())

	sweepTimer := receiveLifecycle(t, timers.created)
	require.True(t, sweepTimer.fire())
	require.Eventually(t, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.swept) == 2 && store.swept[1] == connection.ConversationID()
	}, lifecycleTestTimeout, time.Millisecond)
}

func TestWorkspaceRemovalUsesAttachmentSerializationBoundary(t *testing.T) {
	store := newBrokerAttachmentStore()
	conversationID := sequenceID(8075)
	require.NoError(t, removeImageWorkspace(t.Context(), store, nil, conversationID))
	store.mu.Lock()
	require.Equal(t, []string{conversationID}, store.removed)
	store.mu.Unlock()
}

func TestQueuedImagesAreSharedAndReleasedWhenRemoved(t *testing.T) {
	store := newBrokerAttachmentStore()
	broker, _, session, connection, clientID, _, _, page := turnImageFixture(t, 8100, store, true)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	active := submitCommand(sequenceID(8102), clientID, conversationID, sequenceID(8103), sequenceID(8104), "active", &page)
	result, err := connection.Command(context.Background(), active)
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
	receiveLifecycle(t, session.submitted)
	drainEvents(t, connection.Events(), 3)

	queuedMessageID := sequenceID(8105)
	queued := submitCommand(sequenceID(8106), clientID, conversationID, sequenceID(8107), queuedMessageID, "queued", nil)
	queuedPayload := queued.Payload.(agentprotocol.SubmitPayload)
	queuedPayload.Images = []agentprotocol.ImageReference{{ImageID: sequenceID(8108), Name: "queued.png"}}
	queued.Payload = queuedPayload
	result, err = connection.Command(context.Background(), queued)
	require.NoError(t, err)
	requireCommandResult(t, result, agentprotocol.CommandSucceeded, "")
	queueEvent := receiveLifecycle(t, connection.Events())
	require.Equal(t, "queued.png", queueEvent.Payload.(agentprotocol.QueuePayload).Items[0].Images[0].Name)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))

	remove := agentprotocol.Command{APIVersion: agentprotocol.APIVersion, CommandID: sequenceID(8109), ClientID: clientID, ConversationID: &conversationID, Type: agentprotocol.CommandQueueRemove, Payload: agentprotocol.MessageReferencePayload{MessageID: queuedMessageID}}
	removed, err := connection.Command(context.Background(), remove)
	require.NoError(t, err)
	requireCommandResult(t, removed, agentprotocol.CommandSucceeded, "")
	require.Empty(t, receiveLifecycle(t, connection.Events()).Payload.(agentprotocol.QueuePayload).Items)
	require.Equal(t, removed, receiveLifecycle(t, connection.Events()))
	store.mu.Lock()
	require.Contains(t, store.released, queuedMessageID)
	store.mu.Unlock()
}

func TestQueuedImageCaptionCanBeClearedWithoutChangingImages(t *testing.T) {
	image := provider.ImageInput{ID: sequenceID(8150), Name: "queued.png", MediaType: "image/png", Bytes: 4, Path: filepath.Join("/private/tmp", sequenceID(8150)+".png")}
	descriptor := agentprotocol.ImageDescriptor{ImageID: image.ID, Name: image.Name, MediaType: image.MediaType}
	queue := NewQueue()
	require.NoError(t, queue.Enqueue(QueuedTurn{TurnID: sequenceID(8151), MessageID: sequenceID(8152), Content: provider.TextMessage("caption"), Images: []provider.ImageInput{image}, Descriptors: []agentprotocol.ImageDescriptor{descriptor}}))

	require.NoError(t, queue.Edit(sequenceID(8152), provider.MessageContent{}))
	require.Equal(t, []agentprotocol.QueueItem{{TurnID: sequenceID(8151), MessageID: sequenceID(8152), Content: agentprotocol.MessageContent{Parts: []agentprotocol.MessagePart{}}, Images: []agentprotocol.ImageDescriptor{descriptor}}}, queue.Items())
	request, ok := queue.Dequeue()
	require.True(t, ok)
	require.True(t, request.Content.Empty())
	require.Equal(t, []provider.ImageInput{image}, request.Images)
}

func TestImageCapabilityAndDefiniteProviderRejectionDoNotLeakClaims(t *testing.T) {
	t.Run("unsupported model rejects before claim", func(t *testing.T) {
		store := newBrokerAttachmentStore()
		broker, _, _, connection, clientID, _, _, page := turnImageFixture(t, 8200, store, false)
		defer broker.Close(context.Background())
		conversationID := connection.ConversationID()
		command := submitCommand(sequenceID(8202), clientID, conversationID, sequenceID(8203), sequenceID(8204), "question", &page)
		payload := command.Payload.(agentprotocol.SubmitPayload)
		payload.Images = []agentprotocol.ImageReference{{ImageID: sequenceID(8205), Name: "image.png"}}
		command.Payload = payload
		result, err := connection.Command(context.Background(), command)
		require.NoError(t, err)
		requireCommandResult(t, result, agentprotocol.CommandRejected, agentprotocol.ErrorImageInputUnsupported)
		store.mu.Lock()
		require.Empty(t, store.claims)
		store.mu.Unlock()
	})

	t.Run("definite provider rejection releases claim", func(t *testing.T) {
		store := newBrokerAttachmentStore()
		broker, _, session, connection, clientID, _, _, page := turnImageFixture(t, 8300, store, true)
		defer broker.Close(context.Background())
		session.mu.Lock()
		session.preflightErr = provider.NewProviderError(provider.ErrorContextTooLarge)
		session.mu.Unlock()
		conversationID := connection.ConversationID()
		messageID := sequenceID(8304)
		command := submitCommand(sequenceID(8302), clientID, conversationID, sequenceID(8303), messageID, "question", &page)
		payload := command.Payload.(agentprotocol.SubmitPayload)
		payload.Images = []agentprotocol.ImageReference{{ImageID: sequenceID(8305), Name: "image.png"}}
		command.Payload = payload
		result, err := connection.Command(context.Background(), command)
		require.NoError(t, err)
		requireCommandResult(t, result, agentprotocol.CommandRejected, agentprotocol.ErrorContextTooLarge)
		store.mu.Lock()
		require.Contains(t, store.released, messageID)
		store.mu.Unlock()
	})

	t.Run("unknown acceptance retains claim", func(t *testing.T) {
		store := newBrokerAttachmentStore()
		broker, _, session, connection, clientID, _, _, page := turnImageFixture(t, 8350, store, true)
		defer broker.Close(context.Background())
		session.mu.Lock()
		session.submitErr = provider.NewProviderError(provider.ErrorAcceptanceUnknown)
		session.mu.Unlock()
		conversationID := connection.ConversationID()
		messageID := sequenceID(8354)
		command := submitCommand(sequenceID(8352), clientID, conversationID, sequenceID(8353), messageID, "question", &page)
		payload := command.Payload.(agentprotocol.SubmitPayload)
		payload.Images = []agentprotocol.ImageReference{{ImageID: sequenceID(8355), Name: "image.png"}}
		command.Payload = payload
		result, err := connection.Command(context.Background(), command)
		require.NoError(t, err)
		requireCommandResult(t, result, agentprotocol.CommandRejected, agentprotocol.ErrorAcceptanceOutcomeUnknown)
		store.mu.Lock()
		require.NotContains(t, store.released, messageID)
		store.mu.Unlock()
	})
}

func TestCommandFingerprintIncludesOrderedImageReferences(t *testing.T) {
	conversationID := sequenceID(8400)
	base := agentprotocol.Command{APIVersion: agentprotocol.APIVersion, CommandID: sequenceID(8401), ClientID: sequenceID(8402), ConversationID: &conversationID, Type: agentprotocol.CommandSubmit, Payload: agentprotocol.SubmitPayload{
		TurnID: sequenceID(8403), MessageID: sequenceID(8404), Content: agentprotocol.TextContent("compare"),
		Images: []agentprotocol.ImageReference{{ImageID: sequenceID(8405), Name: "a.png"}, {ImageID: sequenceID(8406), Name: "b.png"}},
	}}
	left, err := commandFingerprint(base)
	require.NoError(t, err)
	changed := base
	payload := changed.Payload.(agentprotocol.SubmitPayload)
	payload.Images = []agentprotocol.ImageReference{payload.Images[1], payload.Images[0]}
	changed.Payload = payload
	right, err := commandFingerprint(changed)
	require.NoError(t, err)
	require.NotEqual(t, left, right)
}

func turnImageFixture(t *testing.T, base uint64, attachments AttachmentStore, supportsImages bool) (*Broker, *repairState, *turnSession, *Connection, string, agentstate.Identity, agentprotocol.Resource, agentprotocol.PageContext) {
	return turnImageFixtureWithTimers(t, base, attachments, supportsImages, RealTimerFactory{})
}

func turnImageFixtureWithTimers(t *testing.T, base uint64, attachments AttachmentStore, supportsImages bool, timers TimerFactory) (*Broker, *repairState, *turnSession, *Connection, string, agentstate.Identity, agentprotocol.Resource, agentprotocol.PageContext) {
	t.Helper()
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(base))
	resource := testResource(identity.CapabilityID)
	page := testPageContext(resource)
	observed := agentstate.Revision{Digest: page.Digest, Revision: agentstate.RevisionInitial, SourceUpdatedAt: resource.UpdatedAt}
	mapping.Current.Observed = &observed
	state := &repairState{mapping: &mapping, promotions: []repairMutation{{outcome: agentstate.CommitApplied, apply: true}, {outcome: agentstate.CommitApplied, apply: true}}, reconciliations: []repairMutation{{outcome: agentstate.CommitApplied, apply: true}, {outcome: agentstate.CommitApplied, apply: true}}}
	session := newTurnSession(mapping.Current.NativeSession.Value())
	session.capabilities.Images = supportsImages
	config := validLifecycleConfig(state, &hardeningDriver{resumeSession: session}, &lockedIDs{next: base + 100})
	config.Attachments = attachments
	config.Timers = timers
	broker, err := New(config)
	require.NoError(t, err)
	clientID := sequenceID(base + 1)
	connected, err := broker.Connect(context.Background(), identity.Origin, observationConnect(clientID, identity.CapabilityID, page.Digest, resource, ""))
	require.NoError(t, err)
	connection := connected.(*Connection)
	snapshot := receiveLifecycle(t, connection.Events())
	require.Equal(t, supportsImages, snapshot.Payload.(agentprotocol.SnapshotPayload).SupportsImages)
	return broker, state, session, connection, clientID, identity, resource, page
}
