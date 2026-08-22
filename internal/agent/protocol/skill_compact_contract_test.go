package protocol_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	"github.com/stretchr/testify/require"
)

func TestV5SkillPartsAreStrictSafeAndProviderNeutral(t *testing.T) {
	content := protocol.MessageContent{Parts: []protocol.MessagePart{
		{Type: protocol.MessagePartSkill, Skill: &protocol.SkillInvocation{ID: idA, Name: "review-helper"}},
		{Type: protocol.MessagePartText, Text: "check this page"},
	}}
	require.NoError(t, content.ValidateCommand())
	require.NoError(t, content.ValidateEvent())
	require.NoError(t, content.ValidateForProvider(protocol.ProviderCodex, false))
	require.NoError(t, content.ValidateForProvider(protocol.ProviderPi, false))

	clone := content.Clone()
	clone.Parts[0].Skill.Name = "changed"
	require.Equal(t, "review-helper", content.Parts[0].Skill.Name)

	encoded, err := json.Marshal(content)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "path")

	conversationID := idC
	command := protocol.Command{APIVersion: protocol.APIVersion, CommandID: idA, ClientID: idB, ConversationID: &conversationID, Type: protocol.CommandSubmit, Payload: protocol.SubmitPayload{TurnID: "DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD", MessageID: "EEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE", Content: content}}
	commandJSON, err := protocol.EncodeCommand(command)
	require.NoError(t, err)
	decoded, err := protocol.DecodeCommand(commandJSON)
	require.NoError(t, err)
	require.Equal(t, content, decoded.Payload.(protocol.SubmitPayload).Content)
	withPath := strings.Replace(string(commandJSON), `"name":"review-helper"`, `"name":"review-helper","path":"/private/skill"`, 1)
	_, err = protocol.DecodeCommand([]byte(withPath))
	require.Error(t, err)

	duplicate := content.Clone()
	duplicate.Parts = append(duplicate.Parts, protocol.MessagePart{Type: protocol.MessagePartSkill, Skill: &protocol.SkillInvocation{ID: idA, Name: "other"}})
	require.Error(t, duplicate.ValidateCommand())

	tooMany := protocol.MessageContent{Parts: make([]protocol.MessagePart, protocol.MaxMessageSkills+1)}
	for index := range tooMany.Parts {
		tooMany.Parts[index] = protocol.MessagePart{Type: protocol.MessagePartSkill, Skill: &protocol.SkillInvocation{ID: strings.Repeat(string(rune('A'+index)), 32), Name: "skill-" + string(rune('a'+index))}}
	}
	require.Error(t, tooMany.ValidateCommand())
}

func TestV5ActiveWorkSkillsAndCompactSnapshotContract(t *testing.T) {
	state := protocol.SkillsReady
	limit := 1
	active := &protocol.ActiveWork{WorkID: idC, Kind: protocol.ActiveWorkCompact, State: protocol.ActiveWorkRunning}
	catalog := []protocol.SkillDescriptor{{ID: idA, Name: "review-helper", DisplayName: "Review helper", Description: "Review this page.", Scope: protocol.SkillScopeRepo}}
	payload := protocol.SnapshotPayload{
		Lifecycle: protocol.LifecycleCompacting, Queue: []protocol.QueueItem{}, ContextState: protocol.ContextAccepted,
		ActiveWork: active, SkillsState: &state, Skills: catalog, MaxSelectedSkills: &limit, SupportsCompact: true, Catalog: []protocol.CatalogModel{},
		BusyPolicy: protocol.BusyTurnQueue, ComposerAdmission: protocol.ComposerSubmit,
	}
	require.NoError(t, payload.ValidateForProvider(protocol.ProviderCodex))
	event := validEvent(payload)
	encoded, err := protocol.EncodeEvent(event)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"active_work":{"work_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","kind":"compact","state":"running"}`)
	require.NotContains(t, string(encoded), "active_turn_id")

	decoded, err := protocol.DecodeEvent(encoded)
	require.NoError(t, err)
	require.Equal(t, payload, decoded.Payload.(protocol.SnapshotPayload))

	wrongLifecycle := payload
	wrongLifecycle.Lifecycle = protocol.LifecycleResponding
	require.Error(t, wrongLifecycle.ValidateForProvider(protocol.ProviderCodex))

	require.NoError(t, payload.ValidateForProvider(protocol.ProviderPi), "capabilities are not provider-name gated")
}

func TestV5CompactAndInterruptCommandsUseDistinctWorkIdentity(t *testing.T) {
	conversationID := idC
	compact := protocol.Command{APIVersion: protocol.APIVersion, CommandID: idA, ClientID: idB, ConversationID: &conversationID, Type: protocol.CommandCompact, Payload: protocol.CompactPayload{WorkID: "DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"}}
	encodedCompact, err := protocol.EncodeCommand(compact)
	require.NoError(t, err)
	require.JSONEq(t, `{"api_version":"5","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"compact","payload":{"work_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"}}`, string(encodedCompact))
	decodedCompact, err := protocol.DecodeCommand(encodedCompact)
	require.NoError(t, err)
	require.Equal(t, compact, decodedCompact)

	interrupt := protocol.Command{APIVersion: protocol.APIVersion, CommandID: idB, ClientID: idA, ConversationID: &conversationID, Type: protocol.CommandInterrupt, Payload: protocol.WorkReferencePayload{WorkID: idC}}
	encodedInterrupt, err := protocol.EncodeCommand(interrupt)
	require.NoError(t, err)
	require.Contains(t, string(encodedInterrupt), `"work_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"`)
	require.NotContains(t, string(encodedInterrupt), "turn_id")
}

func TestV5SkillCatalogAndCompactionEventsAreReplayableAndBounded(t *testing.T) {
	limit := protocol.MaxMessageSkills
	catalog := protocol.SkillCatalogPayload{State: protocol.SkillsReady, Skills: []protocol.SkillDescriptor{{ID: idA, Name: "review-helper", Scope: protocol.SkillScopeUser}}, MaxSelectedSkills: &limit}
	catalogEvent := validEvent(catalog)
	encoded, err := protocol.EncodeEvent(catalogEvent)
	require.NoError(t, err)
	decoded, err := protocol.DecodeEvent(encoded)
	require.NoError(t, err)
	require.Equal(t, catalog, decoded.Payload)

	for _, status := range []protocol.CompactionStatus{protocol.CompactionRunning, protocol.CompactionStopping, protocol.CompactionCompleted, protocol.CompactionInterrupted, protocol.CompactionFailed} {
		payload := protocol.CompactionPayload{WorkID: idC, Status: status}
		event := protocol.Event{APIVersion: protocol.APIVersion, EventID: idA, ConversationID: idB, Type: protocol.EventCompaction, Timestamp: time.Now().UTC(), Payload: payload}
		require.NoError(t, protocol.ValidateReplay([]protocol.Event{event}))
	}

	overflow := catalog
	overflow.Skills[0].Description = strings.Repeat("x", protocol.MaxSkillDescriptionBytes+1)
	_, err = protocol.EncodeEvent(validEvent(overflow))
	require.Error(t, err)
}
