package broker

import (
	"testing"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/protocol"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

func TestInteractionRequestConversionPreservesMultilineCapability(t *testing.T) {
	request := provider.InteractionRequest{
		ID:    "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		Kind:  provider.InteractionMCPElicitation,
		Title: "Edit response",
		Options: []provider.InteractionOption{
			{ID: "accept", Label: "Submit"},
			{ID: "decline", Label: "Decline"},
			{ID: "cancel", Label: "Cancel"},
		},
		Fields: []provider.InteractionField{{
			ID: "value", Label: "Response", Type: provider.InteractionText, Required: true, Multiline: true,
		}},
	}

	converted, err := interactionRequestFromProvider(request)
	require.NoError(t, err)
	require.Len(t, converted.Fields, 1)
	require.True(t, converted.Fields[0].Multiline)
}

func TestSkillCatalogConversionPreservesSelectionLimit(t *testing.T) {
	factory, err := NewEventFactory(testID('A'), &testIDGenerator{ids: []string{testID('B')}}, testClock{now: testTime()})
	require.NoError(t, err)
	providerEvent := provider.NewSkillCatalogEvent(provider.SkillCatalog{
		State: provider.SkillsReady, MaxSelectedSkills: 1,
		Skills: []provider.SkillDescriptor{{ID: testID('C'), Name: "review", Scope: provider.SkillScopeUser}},
	})

	event, err := factory.FromProvider(providerEvent)
	require.NoError(t, err)
	payload, ok := event.Payload.(protocol.SkillCatalogPayload)
	require.True(t, ok)
	require.NotNil(t, payload.MaxSelectedSkills)
	require.Equal(t, 1, *payload.MaxSelectedSkills)
}
