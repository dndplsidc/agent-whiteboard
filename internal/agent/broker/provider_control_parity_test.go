package broker

import (
	"context"
	"testing"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/protocol"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	statepkg "github.com/dndplsidc/agent-whiteboard/internal/agent/state"
	"github.com/stretchr/testify/require"
)

func TestPiCapableSessionRepairsLegacySettingsAndSupportsCompact(t *testing.T) {
	identity, mapping := hardeningMapping(t, "https://example.com", sequenceID(12000))
	mapping.Current.Settings = nil
	mapping.Current.Presentation = nil
	mapping.Current.ModelLabel = "legacy"
	settings := provider.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "high", Speed: provider.SpeedFast}
	presentation := provider.ModelPresentation{ModelDisplayName: "5.6 Sol", Selectable: true}
	base := newTurnSession(mapping.Current.NativeSession.Value())
	base.native.Provider = provider.NamePi
	base.native.Model = settings.Model
	base.native.Settings = &settings
	base.native.Presentation = &presentation
	base.supportsCompact = true
	session := &settingsTurnSession{turnSession: base, catalog: settingsCatalog()}
	state := &repairState{mapping: &mapping}
	driver := &hardeningDriver{resumeSession: session}
	broker, err := New(validLifecycleConfig(state, driver, &lockedIDs{next: 12100}))
	require.NoError(t, err)
	defer broker.Close(context.Background())

	connected, err := broker.Connect(context.Background(), identity.Origin, lifecycleConnect(sequenceID(12001), identity.CapabilityID))
	require.NoError(t, err)
	connection := connected.(*Connection)
	defer connection.Close(context.Background())
	snapshot := receiveLifecycle(t, connection.Events()).Payload.(protocol.SnapshotPayload)
	require.Equal(t, protocol.SettingsVerified, *snapshot.SettingsState)
	require.Equal(t, settings.Model, snapshot.EffectiveSettings.Model)
	require.True(t, snapshot.SupportsCompact)
	require.Equal(t, protocol.BusyTurnPreserveDraft, snapshot.BusyPolicy)
	require.Equal(t, protocol.ComposerSubmit, snapshot.ComposerAdmission)

	state.mu.Lock()
	require.Equal(t, settings, *state.mapping.Current.Settings)
	require.Equal(t, presentation, *state.mapping.Current.Presentation)
	state.mu.Unlock()

	workID := sequenceID(12002)
	result, err := connection.Command(context.Background(), codexCompact(sequenceID(12003), connection.clientID, connection.ConversationID(), workID))
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandSucceeded, "")
	require.Equal(t, workID, receiveLifecycle(t, session.compacted).WorkID)
	drainEvents(t, connection.Events(), 2)
	session.events <- provider.NewCompactEvent(workID, provider.CompactCompleted)
	drainEvents(t, connection.Events(), 2)
}

func TestComposerAdmissionUsesBusyPolicyNotProviderIdentity(t *testing.T) {
	current := &statepkg.Session{}
	for _, identity := range []provider.Name{provider.NamePi, provider.NameCodex} {
		actor := &conversation{identity: statepkg.Identity{Provider: identity}, mapping: statepkg.Mapping{Current: current}, lifecycle: protocol.LifecycleReady, queue: NewQueue()}
		actor.busyPolicy = protocol.BusyTurnQueue
		require.Equal(t, protocol.ComposerSubmit, actor.composerAdmission())
		actor.active = &activeTurn{}
		require.Equal(t, protocol.ComposerQueue, actor.composerAdmission())
		actor.busyPolicy = protocol.BusyTurnPreserveDraft
		require.Equal(t, protocol.ComposerPreserveDraft, actor.composerAdmission())
		actor.busyPolicy = protocol.BusyTurnQueue
		actor.queue.items = make([]queueItem, MaxQueueItems)
		require.Equal(t, protocol.ComposerBlocked, actor.composerAdmission())
	}
}

func TestSkillLimitRejectsBeforeContextOrProviderSideEffects(t *testing.T) {
	broker, state, session, connection, page := codexHTMLPendingFixture(t, 12200)
	defer broker.Close(context.Background())
	defer connection.Close(context.Background())
	skills := []provider.SkillDescriptor{
		{ID: sequenceID(12201), Name: "first", Scope: provider.SkillScopeRepo},
		{ID: sequenceID(12202), Name: "second", Scope: provider.SkillScopeRepo},
	}
	session.skillCatalog = provider.SkillCatalog{State: provider.SkillsReady, Skills: skills, MaxSelectedSkills: 1}
	session.events <- provider.NewSkillCatalogEvent(session.skillCatalog)
	event := receiveLifecycle(t, connection.Events())
	require.Equal(t, protocol.EventSkillCatalog, event.Type)
	payload := event.Payload.(protocol.SkillCatalogPayload)
	require.Equal(t, 1, *payload.MaxSelectedSkills)

	content := protocol.MessageContent{Parts: []protocol.MessagePart{
		{Type: protocol.MessagePartSkill, Skill: &protocol.SkillInvocation{ID: skills[0].ID, Name: skills[0].Name}},
		{Type: protocol.MessagePartSkill, Skill: &protocol.SkillInvocation{ID: skills[1].ID, Name: skills[1].Name}},
	}}
	settings := &protocol.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "high", Speed: protocol.SpeedFast}
	conversationID := connection.ConversationID()
	command := protocol.Command{APIVersion: protocol.APIVersion, CommandID: sequenceID(12203), ClientID: connection.clientID, ConversationID: &conversationID, Type: protocol.CommandSubmit, Payload: protocol.SubmitPayload{
		TurnID: sequenceID(12204), MessageID: sequenceID(12205), Content: content, Context: &page, Settings: settings,
	}}
	result, err := connection.Command(context.Background(), command)
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandRejected, protocol.ErrorSkillUnavailable)
	require.Zero(t, session.providerCalls.Load())
	state.mu.Lock()
	require.Zero(t, state.prepareCalls)
	state.mu.Unlock()
}
