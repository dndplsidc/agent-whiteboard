package broker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/protocol"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	statepkg "github.com/dndplsidc/agent-whiteboard/internal/agent/state"
	"github.com/dndplsidc/agent-whiteboard/internal/common"
	"github.com/stretchr/testify/require"
)

type settingsDriver struct {
	*hardeningDriver
}

func (driver *settingsDriver) Readiness(context.Context) provider.Readiness {
	return provider.Readiness{State: provider.Ready, Provider: provider.NameCodex, Model: "gpt-5.6-sol"}
}

type settingsTurnSession struct {
	*turnSession
	catalog provider.ModelCatalog
}

func (session *settingsTurnSession) SettingsCatalog(context.Context) (provider.ModelCatalog, error) {
	return session.catalog.Clone(), nil
}

func (session *settingsTurnSession) EffectiveSettings(context.Context) (provider.ExecutionSettings, provider.ModelPresentation, error) {
	return *session.native.Settings, *session.native.Presentation, nil
}

func (session *settingsTurnSession) ApplySettings(_ context.Context, settings provider.ExecutionSettings) (provider.ExecutionSettings, provider.ModelPresentation, error) {
	model, err := session.catalog.Resolve(settings)
	if err != nil {
		return provider.ExecutionSettings{}, provider.ModelPresentation{}, err
	}
	presentation := provider.ModelPresentation{ModelDisplayName: model.DisplayName, Selectable: true}
	return settings, presentation, nil
}

func (*settingsTurnSession) BusyTurnPolicy() provider.BusyTurnPolicy {
	return provider.BusyTurnPreserveDraft
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

func codexSettingsSession(refValue string, settings provider.ExecutionSettings) *settingsTurnSession {
	base := newTurnSession(refValue)
	session := &settingsTurnSession{turnSession: base, catalog: settingsCatalog()}
	session.native.Provider = provider.NameCodex
	session.native.Model = settings.Model
	presentation := provider.ModelPresentation{ModelDisplayName: settingsCatalog().Models[0].DisplayName, Selectable: true}
	if settings.Model == "gpt-5.6-luna" {
		presentation.ModelDisplayName = "5.6 Luna"
	}
	session.native.Settings = &settings
	session.native.Presentation = &presentation
	session.capabilities.Images = settings.Model == "gpt-5.6-sol"
	session.skillCatalog = provider.SkillCatalog{State: provider.SkillsReady, Skills: []provider.SkillDescriptor{}, MaxSelectedSkills: provider.MaxMessageSkills}
	session.supportsCompact = true
	return session
}

func codexSettingsFixture(t *testing.T, base uint64) (*Broker, *repairState, *settingsTurnSession, *Connection, statepkg.Identity) {
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
	driver := &settingsDriver{hardeningDriver: &hardeningDriver{resumeSession: session}}
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

func codexCompact(commandID, clientID, conversationID, workID string) protocol.Command {
	return protocol.Command{APIVersion: protocol.APIVersion, CommandID: commandID, ClientID: clientID, ConversationID: &conversationID, Type: protocol.CommandCompact, Payload: protocol.CompactPayload{WorkID: workID}}
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

type failingSettingsIDs struct{}

func (failingSettingsIDs) NewID() (string, error) { return "", errors.New("event id unavailable") }

type fixedSettingsIDs struct{ id string }

func (ids fixedSettingsIDs) NewID() (string, error) { return ids.id, nil }

func settingsPublicationActor(t *testing.T, ids common.IDGenerator) (*conversation, *repairState, provider.ExecutionSettings, provider.ModelPresentation) {
	t.Helper()
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(10330))
	identity.Provider = provider.NameCodex
	mapping.Identity = identity
	initial := provider.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "high", Speed: provider.SpeedFast}
	presentation := provider.ModelPresentation{ModelDisplayName: "5.6 Sol", Selectable: true}
	mapping.Current.ProviderLabel = "codex"
	mapping.Current.ModelLabel = presentation.ModelDisplayName
	mapping.Current.Settings = &initial
	mapping.Current.Presentation = &presentation
	state := &repairState{mapping: &mapping}
	factory, err := NewEventFactory(mapping.Current.ConversationID, ids, testClock{now: testTime()})
	require.NoError(t, err)
	wireCatalog, err := protocolCatalog(settingsCatalog())
	require.NoError(t, err)
	actor := &conversation{
		identity: identity, mapping: cloneMapping(mapping), state: state, factory: factory, replay: NewReplayLog(),
		session: captureSession(codexSettingsSession(mapping.Current.NativeSession.Value(), initial)),
		clock:   testClock{now: testTime()}, domainCatalog: settingsCatalog(), catalog: wireCatalog,
		settingsCapable: true, settingsState: protocol.SettingsVerified, lifecycle: protocol.LifecycleReady,
	}
	actor.applyEffectiveSettings(initial, presentation)
	return actor, state, initial, presentation
}

func TestVerifiedSettingsPreparationFailuresDoNotMutateOrUnblock(t *testing.T) {
	next := provider.ExecutionSettings{Model: "gpt-5.6-luna", Effort: "medium", Speed: provider.SpeedStandard}
	nextPresentation := provider.ModelPresentation{ModelDisplayName: "5.6 Luna", Selectable: true}
	for _, test := range []struct {
		name string
		ids  common.IDGenerator
		seed bool
	}{
		{name: "factory", ids: failingSettingsIDs{}},
		{name: "replay duplicate", ids: fixedSettingsIDs{id: sequenceID(10331)}, seed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			actor, state, initial, initialPresentation := settingsPublicationActor(t, test.ids)
			if test.seed {
				seed := protocol.Event{APIVersion: protocol.APIVersion, EventID: sequenceID(10331), ConversationID: actor.mapping.Current.ConversationID, Type: protocol.EventSettings, Timestamp: testTime(), Payload: protocol.SettingsPayload{SettingsState: protocol.SettingsVerified, EffectiveSettings: actor.presentedEffectiveSettings(&initial, &initialPresentation), Catalog: append([]protocol.CatalogModel{}, actor.catalog...)}}
				require.NoError(t, actor.replay.Append(seed))
			}
			actor.publishSettingsEvent(map[*clientAttachment]struct{}{}, provider.NewVerifiedSettingsEvent("", next, nextPresentation))
			state.mu.Lock()
			require.Equal(t, initial, *state.mapping.Current.Settings)
			require.Equal(t, initialPresentation, *state.mapping.Current.Presentation)
			state.mu.Unlock()
			require.Equal(t, initial, *actor.effectiveSettings)
			require.Equal(t, initialPresentation, *actor.effectivePresentation)
			require.True(t, actor.dispatchBlocked)
			require.Equal(t, protocol.LifecycleUnavailable, actor.lifecycle)
			require.Equal(t, protocol.SettingsUnverified, actor.settingsState)
			for _, entry := range actor.replay.entries {
				payload, ok := entry.Event.Payload.(protocol.SettingsPayload)
				require.False(t, ok && payload.EffectiveSettings != nil && payload.EffectiveSettings.Model == next.Model)
			}
		})
	}
}

func TestIncompatibleVerifiedSettingsEventFailsClosedWithoutPersistence(t *testing.T) {
	broker, state, session, connection, _ := codexSettingsFixture(t, 10350)
	defer broker.Close(context.Background())
	initial := *session.native.Settings
	command := codexSubmit(sequenceID(10352), connection.clientID, connection.ConversationID(), sequenceID(10353), sequenceID(10354), initial)
	gate := make(chan struct{})
	acceptedPresentation := *session.native.Presentation
	session.mu.Lock()
	session.submitGate = gate
	session.submitAccepted = &provider.AcceptedTurn{AcceptedAt: testTime(), Settings: &initial, Presentation: &acceptedPresentation}
	session.mu.Unlock()
	go connection.Command(context.Background(), command)
	receiveLifecycle(t, session.submitted)
	incompatible := provider.ExecutionSettings{Model: "removed-model", Effort: "high", Speed: provider.SpeedStandard}
	session.events <- provider.NewVerifiedSettingsEvent(command.Payload.(protocol.SubmitPayload).TurnID, incompatible, provider.ModelPresentation{ModelDisplayName: "Removed", Selectable: true})
	close(gate)

	var malformed, unverified, unavailable bool
	for index := 0; index < 10 && !(malformed && unverified && unavailable); index++ {
		event := receiveLifecycle(t, connection.Events())
		switch payload := event.Payload.(type) {
		case protocol.ErrorPayload:
			malformed = malformed || payload.Error.Code() == protocol.ErrorProviderMalformedStream
		case protocol.SettingsPayload:
			unverified = unverified || payload.SettingsState == protocol.SettingsUnverified
		case protocol.LifecyclePayload:
			unavailable = unavailable || payload.State == protocol.LifecycleUnavailable
		}
	}
	require.True(t, malformed)
	require.True(t, unverified)
	require.True(t, unavailable)
	state.mu.Lock()
	require.Equal(t, initial, *state.mapping.Current.Settings)
	require.Equal(t, acceptedPresentation, *state.mapping.Current.Presentation)
	state.mu.Unlock()
}

func TestNewPassesValidInitialTupleAndResumeIgnoresConnectPreference(t *testing.T) {
	preference := &protocol.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "high", Speed: protocol.SpeedFast}
	require.Equal(t, "gpt-5.6-sol", compatibleInitialSettings(preference).Model)
	invalid := &protocol.ExecutionSettings{Model: "", Effort: "high", Speed: protocol.SpeedFast}
	require.Nil(t, compatibleInitialSettings(invalid))

	initial := provider.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "high", Speed: provider.SpeedFast}
	native := codexSettingsSession("sessions/new-settings", initial).native
	at := time.Date(2026, 8, 13, 1, 2, 3, 0, time.UTC)
	stored, err := stateSessionFromNative(sequenceID(10400), native, at)
	require.NoError(t, err)
	require.Equal(t, initial, *stored.Settings)
	require.Equal(t, "5.6 Sol", stored.ModelLabel)
}
