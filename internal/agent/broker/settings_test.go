package broker

import (
	"context"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	statepkg "github.com/edocsss/agent-whiteboard/internal/agent/state"
	"github.com/stretchr/testify/require"
)

type settingsDriver struct {
	*hardeningDriver
	catalog provider.ModelCatalog
}

func (driver *settingsDriver) Readiness(context.Context) provider.Readiness {
	return provider.Readiness{State: provider.Ready, Provider: provider.NameCodex, Model: "gpt-5.6-sol"}
}

func (driver *settingsDriver) ModelCatalog(context.Context) (provider.ModelCatalog, error) {
	return driver.catalog.Clone(), nil
}

func settingsCatalog() provider.ModelCatalog {
	return provider.ModelCatalog{Models: []provider.CatalogModel{
		{
			Model: "gpt-5.6-sol", DisplayName: "5.6 Sol", Description: "General coding model", DefaultEffort: "high",
			SupportedReasoningEfforts: []provider.ReasoningEffort{{Value: "medium", Description: "Balanced"}, {Value: "high", Description: "Deeper"}},
			SupportsImages:            true, Default: true, SupportsFast: true,
		},
		{
			Model: "gpt-5.6-luna", DisplayName: "5.6 Luna", Description: "Focused coding model", DefaultEffort: "medium",
			SupportedReasoningEfforts: []provider.ReasoningEffort{{Value: "medium", Description: "Balanced"}},
		},
	}}
}

func codexSettingsSession(refValue string, settings provider.ExecutionSettings) *turnSession {
	session := newTurnSession(refValue)
	session.native.Provider = provider.NameCodex
	session.native.Model = settings.Model
	presentation := provider.ModelPresentation{ModelDisplayName: settingsCatalog().Models[0].DisplayName, Selectable: true}
	if settings.Model == "gpt-5.6-luna" {
		presentation.ModelDisplayName = "5.6 Luna"
	}
	session.native.Settings = &settings
	session.native.Presentation = &presentation
	session.capabilities.Images = settings.Model == "gpt-5.6-sol"
	return session
}

func codexSettingsFixture(t *testing.T, base uint64) (*Broker, *repairState, *turnSession, *Connection, statepkg.Identity) {
	t.Helper()
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(base))
	identity.Provider = provider.NameCodex
	mapping.Identity = identity
	initial := provider.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "high", Speed: provider.SpeedFast}
	presentation := provider.ModelPresentation{ModelDisplayName: "5.6 Sol", Selectable: true}
	mapping.Current.ProviderLabel = "codex"
	mapping.Current.ModelLabel = presentation.ModelDisplayName
	mapping.Current.Settings = &initial
	mapping.Current.Presentation = &presentation
	resource := testResource(identity.CapabilityID)
	committed := statepkg.Revision{Digest: "0000000000000000000000000000000000000000000000000000000000000000", Revision: statepkg.RevisionInitial, SourceUpdatedAt: resource.UpdatedAt}
	mapping.Current.Committed = &committed
	state := &repairState{mapping: &mapping}
	session := codexSettingsSession(mapping.Current.NativeSession.Value(), initial)
	driver := &settingsDriver{hardeningDriver: &hardeningDriver{resumeSession: session}, catalog: settingsCatalog()}
	registry, err := provider.NewRegistry(map[provider.Name]provider.Driver{provider.NameCodex: driver})
	require.NoError(t, err)
	config := validLifecycleConfig(state, nil, &lockedIDs{next: base + 100})
	config.Drivers = registry
	broker, err := New(config)
	require.NoError(t, err)
	command := observationConnect(sequenceID(base+1), identity.CapabilityID, string(make([]byte, 64)), resource, "")
	payload := command.Payload.(protocol.ConnectPayload)
	payload.Provider = protocol.ProviderCodex
	payload.ContextDigest = "0000000000000000000000000000000000000000000000000000000000000000"
	command.Payload = payload
	connected, err := broker.Connect(context.Background(), identity.Origin, command)
	require.NoError(t, err)
	connection := connected.(*Connection)
	snapshot := receiveLifecycle(t, connection.Events()).Payload.(protocol.SnapshotPayload)
	require.Equal(t, protocol.SettingsVerified, *snapshot.SettingsState)
	require.Equal(t, "gpt-5.6-sol", snapshot.EffectiveSettings.Model)
	require.Len(t, snapshot.Catalog, 2)
	return broker, state, session, connection, identity
}

func codexSubmit(commandID, clientID, conversationID, turnID, messageID string, settings provider.ExecutionSettings) protocol.Command {
	wire := protocol.ExecutionSettings{Model: settings.Model, Effort: settings.Effort, Speed: protocol.ExecutionSpeed(settings.Speed)}
	return protocol.Command{APIVersion: protocol.APIVersion, CommandID: commandID, ClientID: clientID, ConversationID: &conversationID, Type: protocol.CommandSubmit, Payload: protocol.SubmitPayload{
		TurnID: turnID, MessageID: messageID, Content: protocol.TextContent("question"), Settings: &wire,
	}}
}

func TestCodexSubmitCapturesSettingsAndPublishesOnlyAfterDurableAcceptance(t *testing.T) {
	broker, state, session, connection, _ := codexSettingsFixture(t, 10000)
	defer broker.Close(context.Background())
	next := provider.ExecutionSettings{Model: "gpt-5.6-luna", Effort: "medium", Speed: provider.SpeedStandard}
	command := codexSubmit(sequenceID(10002), connection.clientID, connection.ConversationID(), sequenceID(10003), sequenceID(10004), next)
	// The fixture has no pending page observation, so the submit is context-free.
	acceptedPresentation := provider.ModelPresentation{ModelDisplayName: "5.6 Luna", Selectable: true}
	session.mu.Lock()
	session.submitAccepted = &provider.AcceptedTurn{AcceptedAt: testTime(), Settings: &next, Presentation: &acceptedPresentation}
	session.mu.Unlock()
	result := make(chan protocol.Event, 1)
	go func() {
		event, _ := connection.Command(context.Background(), command)
		result <- event
	}()
	captured := receiveLifecycle(t, session.submitted)
	require.Equal(t, next, *captured.Settings)
	lifecycle := receiveLifecycle(t, connection.Events())
	require.Equal(t, protocol.EventLifecycle, lifecycle.Type)
	settingsEvent := receiveLifecycle(t, connection.Events())
	require.Equal(t, protocol.EventSettings, settingsEvent.Type)
	settingsPayload := settingsEvent.Payload.(protocol.SettingsPayload)
	require.Equal(t, protocol.SettingsVerified, settingsPayload.SettingsState)
	require.Equal(t, next.Model, settingsPayload.EffectiveSettings.Model)
	require.Equal(t, captured.TurnID, *settingsPayload.AcceptedTurnID)
	commandResult := receiveLifecycle(t, result)
	requireCommandResult(t, commandResult, protocol.CommandSucceeded, "")
	state.mu.Lock()
	require.Equal(t, next, *state.mapping.Current.Settings)
	require.Equal(t, acceptedPresentation, *state.mapping.Current.Presentation)
	state.mu.Unlock()
}

func TestCodexQueueRetainsCapturedTupleAcrossTextEditAndFIFO(t *testing.T) {
	queue := NewQueue()
	first := provider.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "high", Speed: provider.SpeedFast}
	second := provider.ExecutionSettings{Model: "gpt-5.6-luna", Effort: "medium", Speed: provider.SpeedStandard}
	firstPresentation := provider.ModelPresentation{ModelDisplayName: "5.6 Sol", Selectable: true}
	secondPresentation := provider.ModelPresentation{ModelDisplayName: "5.6 Luna", Selectable: true}
	require.NoError(t, queue.Enqueue(QueuedTurn{TurnID: sequenceID(10100), MessageID: sequenceID(10101), Content: provider.TextMessage("first"), Settings: &first, Presentation: &firstPresentation}))
	require.NoError(t, queue.Enqueue(QueuedTurn{TurnID: sequenceID(10102), MessageID: sequenceID(10103), Content: provider.TextMessage("second"), Settings: &second, Presentation: &secondPresentation}))
	require.NoError(t, queue.Edit(sequenceID(10101), provider.TextMessage("edited")))
	items := queue.Items()
	require.Equal(t, "5.6 Sol", items[0].Settings.ModelDisplayName)
	require.Equal(t, protocol.SpeedFast, items[0].Settings.Speed)
	require.Equal(t, "5.6 Luna", items[1].Settings.ModelDisplayName)
	dequeued, ok := queue.Dequeue()
	require.True(t, ok)
	require.Equal(t, first, *dequeued.Settings)
	dequeued, ok = queue.Dequeue()
	require.True(t, ok)
	require.Equal(t, second, *dequeued.Settings)
}

func TestCodexInvalidSettingsRejectBeforeImageClaim(t *testing.T) {
	broker, _, _, connection, _ := codexSettingsFixture(t, 10200)
	defer broker.Close(context.Background())
	invalid := provider.ExecutionSettings{Model: "gpt-5.6-luna", Effort: "high", Speed: provider.SpeedFast}
	command := codexSubmit(sequenceID(10202), connection.clientID, connection.ConversationID(), sequenceID(10203), sequenceID(10204), invalid)
	result, err := connection.Command(context.Background(), command)
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandRejected, protocol.ErrorInvalidModelConfiguration)
}

func TestCodexSettingsProviderEventsFailClosedWhenUnverified(t *testing.T) {
	broker, _, session, connection, _ := codexSettingsFixture(t, 10300)
	defer broker.Close(context.Background())
	command := codexSubmit(sequenceID(10302), connection.clientID, connection.ConversationID(), sequenceID(10303), sequenceID(10304), *session.native.Settings)
	gate := make(chan struct{})
	acceptedPresentation := *session.native.Presentation
	session.mu.Lock()
	session.submitGate = gate
	session.submitAccepted = &provider.AcceptedTurn{AcceptedAt: testTime(), Settings: session.native.Settings, Presentation: &acceptedPresentation}
	session.mu.Unlock()
	go connection.Command(context.Background(), command)
	receiveLifecycle(t, session.submitted)
	session.events <- provider.NewUnverifiedSettingsEvent(command.Payload.(protocol.SubmitPayload).TurnID)
	close(gate)
	found := false
	for index := 0; index < 5; index++ {
		event := receiveLifecycle(t, connection.Events())
		if event.Type != protocol.EventSettings {
			continue
		}
		settings := event.Payload.(protocol.SettingsPayload)
		if settings.SettingsState == protocol.SettingsUnverified {
			found = true
			break
		}
	}
	require.True(t, found)
}

func TestCodexNewUsesVisibleTupleAndResumeIgnoresConnectPreference(t *testing.T) {
	catalog := settingsCatalog()
	stale := &protocol.ExecutionSettings{Model: "removed", Effort: "high", Speed: protocol.SpeedFast}
	require.Nil(t, compatibleInitialSettings(catalog, stale))
	valid := &protocol.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "high", Speed: protocol.SpeedFast}
	require.Equal(t, "gpt-5.6-sol", compatibleInitialSettings(catalog, valid).Model)

	initial := provider.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "high", Speed: provider.SpeedFast}
	native := codexSettingsSession("sessions/new-settings", initial).native
	at := time.Date(2026, 8, 13, 1, 2, 3, 0, time.UTC)
	stored, err := stateSessionFromNative(sequenceID(10400), native, at)
	require.NoError(t, err)
	require.Equal(t, initial, *stored.Settings)
	require.Equal(t, "5.6 Sol", stored.ModelLabel)
}
