package broker

import (
	"context"
	"testing"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/protocol"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	statepkg "github.com/dndplsidc/agent-whiteboard/internal/agent/state"
	"github.com/stretchr/testify/require"
)

type cursorParityDriver struct {
	*hardeningDriver
}

func (*cursorParityDriver) Readiness(context.Context) provider.Readiness {
	return provider.Readiness{State: provider.Ready, Provider: provider.NameCursor, Model: "model"}
}

// cursorTurnParityFixture deliberately reuses the broker's realistic turn,
// settings, state, attachment, and driver fakes. Only their provider identity
// changes: the behavior continues to come from the shared provider interfaces.
func cursorTurnParityFixture(t *testing.T, base uint64, attachments AttachmentStore) (*Broker, *repairState, *settingsTurnSession, *Connection, *brokerAttachmentStore, protocol.PageContext) {
	t.Helper()
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(base))
	identity.Provider = provider.NameCursor
	mapping.Identity = identity
	mapping.Current.ProviderLabel = "Cursor"
	settings := provider.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "high", Speed: provider.SpeedFast}
	presentation := provider.ModelPresentation{ModelDisplayName: "5.6 Sol", Selectable: true}
	mapping.Current.Settings = &settings
	mapping.Current.Presentation = &presentation
	mapping.Current.ModelLabel = presentation.ModelDisplayName
	resource := testResource(identity.CapabilityID)
	page := testPageContext(resource)
	observed := statepkg.Revision{Digest: page.Digest, Revision: statepkg.RevisionInitial, SourceUpdatedAt: resource.UpdatedAt}
	mapping.Current.Observed = &observed
	state := &repairState{
		mapping:         &mapping,
		promotions:      []repairMutation{{outcome: statepkg.CommitApplied, apply: true}, {outcome: statepkg.CommitApplied, apply: true}},
		reconciliations: []repairMutation{{outcome: statepkg.CommitApplied, apply: true}, {outcome: statepkg.CommitApplied, apply: true}},
	}
	baseSession := newTurnSession(mapping.Current.NativeSession.Value())
	baseSession.native.Provider = provider.NameCursor
	baseSession.native.Model = settings.Model
	baseSession.native.Settings = &settings
	baseSession.native.Presentation = &presentation
	baseSession.capabilities.Images = true
	session := &settingsTurnSession{turnSession: baseSession, catalog: settingsCatalog()}
	driver := &cursorParityDriver{hardeningDriver: &hardeningDriver{resumeSession: session}}
	registry, err := provider.NewRegistry(map[provider.Name]provider.Driver{provider.NameCursor: driver})
	require.NoError(t, err)
	config := validLifecycleConfig(state, nil, &lockedIDs{next: base + 100})
	config.Drivers = registry
	config.Attachments = attachments
	broker, err := New(config)
	require.NoError(t, err)
	command := observationConnect(sequenceID(base+1), identity.CapabilityID, page.Digest, resource, "")
	payload := command.Payload.(protocol.ConnectPayload)
	payload.Provider = protocol.ProviderCursor
	command.Payload = payload
	connected, err := broker.Connect(context.Background(), identity.Origin, command)
	require.NoError(t, err)
	connection := connected.(*Connection)
	snapshot := receiveLifecycle(t, connection.Events()).Payload.(protocol.SnapshotPayload)
	require.Equal(t, provider.NameCursor, connection.actor.identity.Provider)
	require.Equal(t, provider.NameCursor, session.NativeSession().Provider)
	require.Equal(t, protocol.SettingsVerified, *snapshot.SettingsState)
	require.True(t, snapshot.SupportsImages)
	require.Equal(t, protocol.BusyTurnPreserveDraft, snapshot.BusyPolicy)
	store, _ := attachments.(*brokerAttachmentStore)
	return broker, state, session, connection, store, page
}

func TestCursorTurnActorsUseSharedSettingsImagesHistoryInteractionAndBusyInterfaces(t *testing.T) {
	attachments := newBrokerAttachmentStore()
	broker, _, session, connection, store, page := cursorTurnParityFixture(t, 13000, attachments)
	defer broker.Close(context.Background())
	conversationID := connection.ConversationID()
	turnID, messageID := sequenceID(13002), sequenceID(13003)
	settings := provider.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "high", Speed: provider.SpeedFast}
	presentation := provider.ModelPresentation{ModelDisplayName: "5.6 Sol", Selectable: true}
	session.mu.Lock()
	session.submitAccepted = &provider.AcceptedTurn{AcceptedAt: testTime(), Settings: &settings, Presentation: &presentation}
	session.mu.Unlock()
	command := codexSubmit(sequenceID(13004), connection.clientID, conversationID, turnID, messageID, settings)
	payload := command.Payload.(protocol.SubmitPayload)
	payload.Context = &page
	payload.Images = []protocol.ImageReference{{ImageID: sequenceID(13005), Name: "cursor.png"}}
	command.Payload = payload
	result, err := connection.Command(context.Background(), command)
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandSucceeded, "")
	submitted := receiveLifecycle(t, session.submitted)
	require.Equal(t, turnID, submitted.TurnID)
	require.Equal(t, settings, *submitted.Settings)
	require.Equal(t, sequenceID(13005), submitted.Images[0].ID)
	store.mu.Lock()
	require.Equal(t, provider.NameCursor, store.claims[0].Provider)
	store.mu.Unlock()
	for published := (protocol.Event{}); published.EventID != result.EventID; {
		published = receiveLifecycle(t, connection.Events())
	}

	busy := submitCommand(sequenceID(13006), connection.clientID, conversationID, sequenceID(13007), sequenceID(13008), "keep this draft", nil)
	busyResult, err := connection.Command(context.Background(), busy)
	require.NoError(t, err)
	requireCommandResult(t, busyResult, protocol.CommandRejected, protocol.ErrorActiveTurnConflict)
	require.Equal(t, busyResult, receiveLifecycle(t, connection.Events()))
	select {
	case unexpected := <-session.submitted:
		t.Fatalf("preserved draft reached provider: %#v", unexpected)
	default:
	}

	requestID := sequenceID(13009)
	session.events <- provider.NewInteractionRequestEvent(provider.InteractionRequest{ID: requestID, TurnID: turnID, Kind: provider.InteractionCommandApproval, Title: "Approve", Options: []provider.InteractionOption{{ID: "accept", Label: "Accept"}}})
	require.Equal(t, protocol.EventInteractionRequest, receiveLifecycle(t, connection.Events()).Type)
	response, err := connection.Command(context.Background(), interactionResponseCommand(sequenceID(13010), connection.clientID, conversationID, requestID, protocol.InteractionCommandApproval, "accept"))
	require.NoError(t, err)
	requireCommandResult(t, response, protocol.CommandSucceeded, "")
	require.Equal(t, "accept", receiveLifecycle(t, session.responded).OptionID)
	require.Equal(t, protocol.EventInteractionResolved, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, response, receiveLifecycle(t, connection.Events()))

	session.events <- provider.NewCompletionEvent(turnID)
	require.Equal(t, protocol.EventCompletion, receiveLifecycle(t, connection.Events()).Type)
	require.Equal(t, protocol.LifecycleReady, receiveLifecycle(t, connection.Events()).Payload.(protocol.LifecyclePayload).State)
	session.mu.Lock()
	session.historyPage = provider.HistoryPage{Items: []provider.HistoryItem{{TurnID: turnID, MessageID: messageID, Role: provider.HistoryUser, Content: provider.TextMessage("question"), CreatedAt: testTime()}}}
	session.mu.Unlock()
	history := historyCommand(sequenceID(13011), connection.clientID, conversationID, protocol.PageRequestPayload{Limit: 1})
	historyResult, err := connection.Command(context.Background(), history)
	require.NoError(t, err)
	requireCommandResult(t, historyResult, protocol.CommandSucceeded, "")
	timeline := receiveLifecycle(t, connection.Events()).Payload.(protocol.TimelinePayload)
	require.Equal(t, "question", timeline.Items[0].Content.Parts[0].Text)
	require.Equal(t, historyResult, receiveLifecycle(t, connection.Events()))
}

func TestCursorArchiveReplayNewAndRestoreUseSharedHandoffInterfaces(t *testing.T) {
	t.Run("archive replay and New", func(t *testing.T) {
		broker, state, driver, connection, identity := archiveFixtureForProvider(t, 13200, provider.NameCursor, nil)
		conversationID := connection.ConversationID()
		list := archiveListCommand(sequenceID(13211), connection.clientID, conversationID, "", 2)
		result, err := connection.Command(context.Background(), list)
		require.NoError(t, err)
		requireCommandResult(t, result, protocol.CommandSucceeded, "")
		history := receiveLifecycle(t, connection.Events())
		require.Equal(t, protocol.EventHistory, history.Type)
		require.Len(t, history.Payload.(protocol.HistoryPayload).Items, 2)
		require.Equal(t, result, receiveLifecycle(t, connection.Events()))
		resync := protocol.Command{APIVersion: protocol.APIVersion, CommandID: sequenceID(13213), ClientID: connection.clientID, ConversationID: &conversationID, Type: protocol.CommandResync, Payload: protocol.ResyncPayload{AfterEventID: result.EventID}}
		resyncResult, err := connection.Command(context.Background(), resync)
		require.NoError(t, err)
		requireCommandResult(t, resyncResult, protocol.CommandSucceeded, "")
		require.Equal(t, protocol.EventSnapshot, receiveLifecycle(t, connection.Events()).Type)
		require.Equal(t, resyncResult, receiveLifecycle(t, connection.Events()))
		duplicate, err := connection.Command(context.Background(), list)
		require.NoError(t, err)
		require.Equal(t, result, duplicate)
		require.Equal(t, result, receiveLifecycle(t, connection.Events()))

		candidate := newTurnSession("sessions/cursor-new")
		candidate.native.Provider = provider.NameCursor
		driver.mu.Lock()
		driver.createSessions = []provider.Session{candidate}
		driver.mu.Unlock()
		newResult, err := connection.Command(context.Background(), newCommand(sequenceID(13212), connection.clientID, conversationID))
		require.NoError(t, err)
		requireCommandResult(t, newResult, protocol.CommandSucceeded, "")
		state.repairState.mu.Lock()
		require.Equal(t, provider.NameCursor, state.mapping.Identity.Provider)
		require.Equal(t, conversationID, state.mapping.Archives[len(state.mapping.Archives)-1].ConversationID)
		state.repairState.mu.Unlock()
		driver.mu.Lock()
		require.Equal(t, provider.NameCursor, driver.createRequests[0].Provider)
		driver.mu.Unlock()
		require.NoError(t, broker.Close(context.Background()))
		require.Equal(t, provider.NameCursor, identity.Provider)
		require.EqualValues(t, 1, candidate.shutdowns.Load())
	})

	t.Run("archive restore", func(t *testing.T) {
		broker, state, driver, connection, _ := archiveFixtureForProvider(t, 13300, provider.NameCursor, nil)
		archiveID := sequenceID(13302)
		candidate := newTurnSession("sessions/archive-two")
		candidate.native.Provider = provider.NameCursor
		driver.mu.Lock()
		driver.resumeSessions = []provider.Session{nil, candidate}
		driver.mu.Unlock()
		result, err := connection.Command(context.Background(), restoreCommand(sequenceID(13311), connection.clientID, connection.ConversationID(), archiveID))
		require.NoError(t, err)
		requireCommandResult(t, result, protocol.CommandSucceeded, "")
		state.repairState.mu.Lock()
		require.Equal(t, archiveID, state.mapping.Current.ConversationID)
		require.Equal(t, provider.NameCursor, state.mapping.Identity.Provider)
		state.repairState.mu.Unlock()
		driver.mu.Lock()
		require.Equal(t, provider.NameCursor, driver.resumeRequests[len(driver.resumeRequests)-1].Provider)
		driver.mu.Unlock()
		require.NoError(t, broker.Close(context.Background()))
	})
}

func TestCursorPreparedTurnRecoveryUsesSharedReconcileAndShutdown(t *testing.T) {
	identity, mapping, revision := preparedRepairMapping(t, statepkg.CommitPrepared)
	identity.Provider = provider.NameCursor
	mapping.Identity = identity
	mapping.Current.ProviderLabel = "Cursor"
	settings := provider.ExecutionSettings{Model: "model", Effort: "high", Speed: provider.SpeedStandard}
	presentation := provider.ModelPresentation{ModelDisplayName: "Model", Selectable: false}
	mapping.Current.Settings = &settings
	mapping.Current.Presentation = &presentation
	mapping.Current.ModelLabel = presentation.ModelDisplayName
	state := &repairState{mapping: &mapping, reconciliations: []repairMutation{{outcome: statepkg.CommitApplied, apply: true}}}
	session := newHardeningSession(mapping.Current.NativeSession.Value())
	session.native.Provider = provider.NameCursor
	session.reconcileState = provider.TurnAccepted
	driver := &cursorParityDriver{hardeningDriver: &hardeningDriver{resumeSession: session}}
	registry, err := provider.NewRegistry(map[provider.Name]provider.Driver{provider.NameCursor: driver})
	require.NoError(t, err)
	config := validLifecycleConfig(state, nil, &lockedIDs{next: 13100})
	config.Drivers = registry
	broker, err := New(config)
	require.NoError(t, err)
	connection, err := broker.Connect(context.Background(), identity.Origin, lifecycleProviderConnect(sequenceID(13101), identity.CapabilityID, protocol.ProviderCursor))
	require.NoError(t, err)
	snapshot := receiveLifecycle(t, connection.Events()).Payload.(protocol.SnapshotPayload)
	require.Equal(t, provider.NameCursor, connection.(*Connection).actor.identity.Provider)
	require.Equal(t, provider.NameCursor, session.NativeSession().Provider)
	require.Equal(t, protocol.ContextUnchanged, snapshot.ContextState)
	require.EqualValues(t, 1, session.reconciliations.Load())
	require.Zero(t, session.submissions.Load())
	state.mu.Lock()
	require.Equal(t, revision, *state.mapping.Current.Committed)
	require.Nil(t, state.mapping.Current.PreparedCommit)
	state.mu.Unlock()
	require.NoError(t, broker.Close(context.Background()))
	require.EqualValues(t, 1, session.shutdowns.Load())
}
