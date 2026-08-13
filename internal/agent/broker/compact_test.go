package broker

import (
	"context"
	"testing"

	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

func TestCodexSnapshotPublishesSafeSkillsAndCompactCapability(t *testing.T) {
	broker, _, session, connection, identity := codexSettingsFixture(t, 8200)
	defer broker.Close(context.Background())
	defer connection.Close(context.Background())

	skill := provider.SkillDescriptor{ID: sequenceID(8202), Name: "review-helper", DisplayName: "Review helper", Description: "Review the current work", Scope: provider.SkillScopeRepo}
	session.events <- provider.NewSkillCatalogEvent(provider.SkillCatalog{State: provider.SkillsReady, Skills: []provider.SkillDescriptor{skill}})
	catalogEvent := receiveLifecycle(t, connection.Events())
	require.Equal(t, protocol.EventSkillCatalog, catalogEvent.Type)
	catalog := catalogEvent.Payload.(protocol.SkillCatalogPayload)
	require.Equal(t, protocol.SkillsReady, catalog.State)
	require.Equal(t, []protocol.SkillDescriptor{{ID: skill.ID, Name: skill.Name, DisplayName: skill.DisplayName, Description: skill.Description, Scope: protocol.SkillScopeRepo}}, catalog.Skills)

	secondClientID := sequenceID(8203)
	connect := observationConnect(secondClientID, identity.CapabilityID, "0000000000000000000000000000000000000000000000000000000000000000", testResource(identity.CapabilityID), "")
	connectPayload := connect.Payload.(protocol.ConnectPayload)
	connectPayload.Provider = protocol.ProviderCodex
	connect.Payload = connectPayload
	connected, err := broker.Connect(context.Background(), identity.Origin, connect)
	require.NoError(t, err)
	defer connected.Close(context.Background())
	snapshot := receiveLifecycle(t, connected.Events()).Payload.(protocol.SnapshotPayload)
	require.NotNil(t, snapshot.SkillsState)
	require.Equal(t, protocol.SkillsReady, *snapshot.SkillsState)
	require.Equal(t, catalog.Skills, snapshot.Skills)
	require.True(t, snapshot.SupportsCompact)
}

func TestCodexSkillValidationRejectsStaleSelectionBeforeProviderWork(t *testing.T) {
	broker, state, session, connection, _ := codexSettingsFixture(t, 8210)
	defer broker.Close(context.Background())
	defer connection.Close(context.Background())
	conversationID := connection.ConversationID()
	clientID := sequenceID(8211)
	skill := provider.SkillDescriptor{ID: sequenceID(8212), Name: "review-helper", Scope: provider.SkillScopeRepo}
	session.skillCatalog = provider.SkillCatalog{State: provider.SkillsReady, Skills: []provider.SkillDescriptor{skill}}
	session.events <- provider.NewSkillCatalogEvent(session.skillCatalog)
	require.Equal(t, protocol.EventSkillCatalog, receiveLifecycle(t, connection.Events()).Type)

	content := protocol.MessageContent{Parts: []protocol.MessagePart{{Type: protocol.MessagePartSkill, Skill: &protocol.SkillInvocation{ID: skill.ID, Name: "removed-helper"}}}}
	command := codexSubmit(sequenceID(8213), clientID, conversationID, sequenceID(8214), sequenceID(8215), provider.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "high", Speed: provider.SpeedFast})
	command.Payload = protocol.SubmitPayload{TurnID: sequenceID(8214), MessageID: sequenceID(8215), Content: content, Settings: command.Payload.(protocol.SubmitPayload).Settings}
	result, err := connection.Command(context.Background(), command)
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandRejected, protocol.ErrorSkillUnavailable)
	require.EqualValues(t, 0, session.submissions.Load())
	require.EqualValues(t, 0, session.providerCalls.Load())
	require.Nil(t, state.mapping.Current.PreparedCommit)
}

func TestCodexBusySubmitIsRejectedWithoutQueueAdmission(t *testing.T) {
	broker, _, session, connection, _ := codexSettingsFixture(t, 8220)
	defer broker.Close(context.Background())
	defer connection.Close(context.Background())
	conversationID := connection.ConversationID()
	clientID := sequenceID(8221)
	settings := provider.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "high", Speed: provider.SpeedFast}
	first := codexSubmit(sequenceID(8222), clientID, conversationID, sequenceID(8223), sequenceID(8224), settings)
	_, err := connection.Command(context.Background(), first)
	require.NoError(t, err)
	receiveLifecycle(t, session.submitted)
	drainEvents(t, connection.Events(), 3)

	second := codexSubmit(sequenceID(8225), clientID, conversationID, sequenceID(8226), sequenceID(8227), settings)
	result, err := connection.Command(context.Background(), second)
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandRejected, protocol.ErrorActiveTurnConflict)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))
	require.EqualValues(t, 1, session.submissions.Load())
}

func TestCodexCompactLifecycleAndStopAreSharedAcrossTabs(t *testing.T) {
	broker, _, session, first, identity := codexSettingsFixture(t, 8230)
	defer broker.Close(context.Background())
	defer first.Close(context.Background())
	conversationID := first.ConversationID()
	firstClientID := sequenceID(8231)
	secondClientID := sequenceID(8232)
	connect := observationConnect(secondClientID, identity.CapabilityID, "0000000000000000000000000000000000000000000000000000000000000000", testResource(identity.CapabilityID), "")
	connectPayload := connect.Payload.(protocol.ConnectPayload)
	connectPayload.Provider = protocol.ProviderCodex
	connect.Payload = connectPayload
	connected, err := broker.Connect(context.Background(), identity.Origin, connect)
	require.NoError(t, err)
	second := connected.(*Connection)
	defer second.Close(context.Background())
	receiveLifecycle(t, second.Events())

	workID := sequenceID(8233)
	command := codexCompact(sequenceID(8234), firstClientID, conversationID, workID)
	result, err := first.Command(context.Background(), command)
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandSucceeded, "")
	require.Equal(t, workID, receiveLifecycle(t, session.compacted).WorkID)
	for _, events := range []<-chan protocol.Event{first.Events(), second.Events()} {
		lifecycle := receiveLifecycle(t, events).Payload.(protocol.LifecyclePayload)
		require.Equal(t, protocol.LifecycleCompacting, lifecycle.State)
		require.NotNil(t, lifecycle.ActiveWork)
		require.Equal(t, protocol.ActiveWorkCompact, lifecycle.ActiveWork.Kind)
		require.Equal(t, workID, lifecycle.ActiveWork.WorkID)
		compaction := receiveLifecycle(t, events).Payload.(protocol.CompactionPayload)
		require.Equal(t, protocol.CompactionRunning, compaction.Status)
	}
	require.Equal(t, result, receiveLifecycle(t, first.Events()))

	interrupt := protocol.Command{APIVersion: protocol.APIVersion, CommandID: sequenceID(8235), ClientID: secondClientID, ConversationID: &conversationID, Type: protocol.CommandInterrupt, Payload: protocol.WorkReferencePayload{WorkID: workID}}
	interruptResult, err := second.Command(context.Background(), interrupt)
	require.NoError(t, err)
	requireCommandResult(t, interruptResult, protocol.CommandSucceeded, "")
	require.Equal(t, workID, receiveLifecycle(t, session.compactInterrupted).WorkID)
	for _, events := range []<-chan protocol.Event{first.Events(), second.Events()} {
		lifecycle := receiveLifecycle(t, events).Payload.(protocol.LifecyclePayload)
		require.Equal(t, protocol.ActiveWorkStopping, lifecycle.ActiveWork.State)
		compaction := receiveLifecycle(t, events).Payload.(protocol.CompactionPayload)
		require.Equal(t, protocol.CompactionStopping, compaction.Status)
	}
	require.Equal(t, interruptResult, receiveLifecycle(t, second.Events()))

	session.events <- provider.NewCompactEvent(workID, provider.CompactInterrupted)
	for _, events := range []<-chan protocol.Event{first.Events(), second.Events()} {
		terminal := receiveLifecycle(t, events).Payload.(protocol.CompactionPayload)
		require.Equal(t, protocol.CompactionInterrupted, terminal.Status)
		lifecycle := receiveLifecycle(t, events).Payload.(protocol.LifecyclePayload)
		require.Equal(t, protocol.LifecycleInterrupted, lifecycle.State)
		require.Nil(t, lifecycle.ActiveWork)
	}
}

func TestCodexCompactUnsupportedDowngradesRuntimeCapability(t *testing.T) {
	broker, _, session, connection, _ := codexSettingsFixture(t, 8240)
	defer broker.Close(context.Background())
	defer connection.Close(context.Background())
	conversationID := connection.ConversationID()
	clientID := sequenceID(8241)
	session.compactErr = provider.NewProviderError(provider.ErrorCompactUnsupported)
	result, err := connection.Command(context.Background(), codexCompact(sequenceID(8242), clientID, conversationID, sequenceID(8243)))
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandRejected, protocol.ErrorCompactUnsupported)
	require.Equal(t, sequenceID(8243), receiveLifecycle(t, session.compacted).WorkID)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))

	result, err = connection.Command(context.Background(), codexCompact(sequenceID(8244), clientID, conversationID, sequenceID(8245)))
	require.NoError(t, err)
	requireCommandResult(t, result, protocol.CommandRejected, protocol.ErrorCompactUnsupported)
	require.Equal(t, result, receiveLifecycle(t, connection.Events()))
	select {
	case <-session.compacted:
		t.Fatal("compact reached provider after capability downgrade")
	default:
	}
}
