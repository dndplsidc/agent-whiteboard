package protocol_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/protocol"
	"github.com/stretchr/testify/require"
)

func TestV5ProviderControlSnapshotIsStrictAndProviderNeutral(t *testing.T) {
	skillsState := protocol.SkillsReady
	limit := 1
	for _, providerName := range []protocol.ProviderName{protocol.ProviderPi, protocol.ProviderCodex} {
		payload := protocol.SnapshotPayload{
			Lifecycle: protocol.LifecycleReady, Queue: []protocol.QueueItem{}, ContextState: protocol.ContextAccepted,
			Catalog: []protocol.CatalogModel{}, SkillsState: &skillsState, Skills: []protocol.SkillDescriptor{}, MaxSelectedSkills: &limit,
			BusyPolicy: protocol.BusyTurnQueue, ComposerAdmission: protocol.ComposerSubmit,
		}
		require.NoError(t, payload.ValidateForProvider(providerName))
		encoded, err := protocol.EncodeEvent(validEvent(payload))
		require.NoError(t, err)
		for _, field := range []string{"max_selected_skills", "busy_policy", "composer_admission"} {
			without := removeJSONField(t, encoded, field)
			_, err := protocol.DecodeEvent(without)
			require.ErrorIs(t, err, protocol.ErrInvalidMessage, field)
		}
	}
}

func TestV5SkillLimitsBusyPolicyAndAdmissionRejectInvalidValues(t *testing.T) {
	ready := protocol.SkillsReady
	limit := protocol.MaxMessageSkills
	base := protocol.SnapshotPayload{
		Lifecycle: protocol.LifecycleReady, Queue: []protocol.QueueItem{}, ContextState: protocol.ContextAccepted,
		Catalog: []protocol.CatalogModel{}, SkillsState: &ready, Skills: []protocol.SkillDescriptor{}, MaxSelectedSkills: &limit,
		BusyPolicy: protocol.BusyTurnPreserveDraft, ComposerAdmission: protocol.ComposerPreserveDraft,
	}
	require.NoError(t, base.ValidateForProvider(protocol.ProviderPi))

	badLimit := 0
	base.MaxSelectedSkills = &badLimit
	require.Error(t, base.ValidateForProvider(protocol.ProviderPi))
	badLimit = protocol.MaxMessageSkills + 1
	require.Error(t, base.ValidateForProvider(protocol.ProviderPi))

	base.MaxSelectedSkills = &limit
	base.BusyPolicy = "native"
	require.Error(t, base.ValidateForProvider(protocol.ProviderPi))
	base.BusyPolicy = protocol.BusyTurnQueue
	base.ComposerAdmission = "native"
	require.Error(t, base.ValidateForProvider(protocol.ProviderPi))
	base.ComposerAdmission = protocol.ComposerPreserveDraft
	require.Error(t, base.ValidateForProvider(protocol.ProviderPi))
	base.BusyPolicy = protocol.BusyTurnPreserveDraft
	base.ComposerAdmission = protocol.ComposerQueue
	require.Error(t, base.ValidateForProvider(protocol.ProviderPi))

	unavailable := protocol.SkillsUnavailable
	base.SkillsState = &unavailable
	base.MaxSelectedSkills = nil
	base.ComposerAdmission = protocol.ComposerBlocked
	require.NoError(t, base.ValidateForProvider(protocol.ProviderCodex))
	base.MaxSelectedSkills = &limit
	require.Error(t, base.ValidateForProvider(protocol.ProviderCodex))
}

func TestV5SkillAndInteractionEventsCarryStrictGenericMetadata(t *testing.T) {
	limit := 1
	catalog := protocol.SkillCatalogPayload{State: protocol.SkillsReady, Skills: []protocol.SkillDescriptor{}, MaxSelectedSkills: &limit}
	encodedCatalog, err := protocol.EncodeEvent(validEvent(catalog))
	require.NoError(t, err)
	_, err = protocol.DecodeEvent(removeJSONField(t, encodedCatalog, "max_selected_skills"))
	require.ErrorIs(t, err, protocol.ErrInvalidMessage)

	deadline := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	request := protocol.InteractionRequestPayload{
		RequestID: idC, Kind: protocol.InteractionMCPElicitation, Title: "Edit input", LocalDeadline: &deadline,
		Options: []protocol.InteractionOption{{ID: "accept", Label: "Accept"}, {ID: "decline", Label: "Decline"}, {ID: "cancel", Label: "Cancel"}},
		Fields:  []protocol.InteractionField{{ID: "body", Label: "Body", Type: protocol.InteractionText, Multiline: true}},
	}
	encodedRequest, err := protocol.EncodeEvent(validEvent(request))
	require.NoError(t, err)
	decoded, err := protocol.DecodeEvent(encodedRequest)
	require.NoError(t, err)
	require.Equal(t, request, decoded.Payload)
	_, err = protocol.DecodeEvent(removeJSONField(t, encodedRequest, "local_deadline"))
	require.ErrorIs(t, err, protocol.ErrInvalidMessage)
	_, err = protocol.DecodeEvent(removeInteractionFieldJSONField(t, encodedRequest, "multiline"))
	require.ErrorIs(t, err, protocol.ErrInvalidMessage)

	nonUTC := time.Date(2026, 8, 21, 12, 0, 0, 0, time.FixedZone("offset", 3600))
	request.LocalDeadline = &nonUTC
	_, err = protocol.EncodeEvent(validEvent(request))
	require.ErrorIs(t, err, protocol.ErrInvalidMessage)

	bad := strings.Replace(string(encodedRequest), `"type":"text","required":false`, `"type":"boolean","required":false`, 1)
	_, err = protocol.DecodeEvent([]byte(bad))
	require.ErrorIs(t, err, protocol.ErrInvalidMessage)
}

func removeJSONField(t *testing.T, encoded []byte, field string) []byte {
	t.Helper()
	var document map[string]any
	require.NoError(t, json.Unmarshal(encoded, &document))
	delete(document["payload"].(map[string]any), field)
	result, err := json.Marshal(document)
	require.NoError(t, err)
	return result
}

func removeInteractionFieldJSONField(t *testing.T, encoded []byte, field string) []byte {
	t.Helper()
	var document map[string]any
	require.NoError(t, json.Unmarshal(encoded, &document))
	fields := document["payload"].(map[string]any)["fields"].([]any)
	delete(fields[0].(map[string]any), field)
	result, err := json.Marshal(document)
	require.NoError(t, err)
	return result
}
