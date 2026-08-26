package provider_test

import (
	"context"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

func TestSessionSettingsCapabilityIsProviderNeutral(t *testing.T) {
	var capability provider.SettingsSession = (*settingsSessionContract)(nil)
	catalog, err := capability.SettingsCatalog(context.Background())
	require.NoError(t, err)
	require.NoError(t, catalog.Validate())

	requested := provider.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "high", Speed: provider.SpeedStandard}
	effective, presentation, err := capability.EffectiveSettings(context.Background())
	require.NoError(t, err)
	require.NoError(t, effective.Validate())
	require.NoError(t, presentation.Validate())

	effective, presentation, err = capability.ApplySettings(context.Background(), requested)
	require.NoError(t, err)
	require.Equal(t, requested, effective)
	require.NoError(t, presentation.Validate())
}

func TestSkillSelectionLimitAndBusyPolicyAreBounded(t *testing.T) {
	ready := provider.SkillCatalog{State: provider.SkillsReady, MaxSelectedSkills: 1, Skills: []provider.SkillDescriptor{}}
	require.NoError(t, ready.Validate())
	ready.MaxSelectedSkills = provider.MaxMessageSkills
	require.NoError(t, ready.Validate())
	ready.MaxSelectedSkills = 0
	require.Error(t, ready.Validate())
	ready.MaxSelectedSkills = provider.MaxMessageSkills + 1
	require.Error(t, ready.Validate())

	unavailable := provider.SkillCatalog{State: provider.SkillsUnavailable, Skills: []provider.SkillDescriptor{}}
	require.NoError(t, unavailable.Validate())
	unavailable.MaxSelectedSkills = 1
	require.Error(t, unavailable.Validate())

	for _, policy := range []provider.BusyTurnPolicy{provider.BusyTurnQueue, provider.BusyTurnPreserveDraft} {
		require.True(t, policy.Valid())
	}
	require.False(t, provider.BusyTurnPolicy("provider_default").Valid())
	_, capable := any((*busySessionContract)(nil)).(provider.BusyTurnSession)
	require.True(t, capable)
}

func TestInteractionLocalDeadlineAndMultilineMetadataAreValidated(t *testing.T) {
	deadline := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	request := provider.InteractionRequest{
		ID: idC, Kind: provider.InteractionMCPElicitation, Title: "Edit input", LocalDeadline: &deadline,
		Options: []provider.InteractionOption{{ID: "accept", Label: "Accept"}, {ID: "decline", Label: "Decline"}, {ID: "cancel", Label: "Cancel"}},
		Fields:  []provider.InteractionField{{ID: "body", Label: "Body", Type: provider.InteractionText, Multiline: true}},
	}
	require.NoError(t, request.Validate())

	badDeadline := time.Time{}
	request.LocalDeadline = &badDeadline
	require.Error(t, request.Validate())
	nonUTC := time.Date(2026, 8, 21, 12, 0, 0, 0, time.FixedZone("offset", 3600))
	request.LocalDeadline = &nonUTC
	require.Error(t, request.Validate())
	request.LocalDeadline = &deadline
	request.Fields[0].Type = provider.InteractionBoolean
	require.Error(t, request.Validate(), "multiline is valid only for text fields")
}

type settingsSessionContract struct{}

func (*settingsSessionContract) SettingsCatalog(context.Context) (provider.ModelCatalog, error) {
	return validModelCatalog(), nil
}
func (*settingsSessionContract) EffectiveSettings(context.Context) (provider.ExecutionSettings, provider.ModelPresentation, error) {
	return provider.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "medium", Speed: provider.SpeedStandard}, provider.ModelPresentation{ModelDisplayName: "5.6 Sol", Selectable: true}, nil
}
func (*settingsSessionContract) ApplySettings(_ context.Context, settings provider.ExecutionSettings) (provider.ExecutionSettings, provider.ModelPresentation, error) {
	return settings, provider.ModelPresentation{ModelDisplayName: "5.6 Sol", Selectable: true}, nil
}

type busySessionContract struct{}

func (*busySessionContract) BusyTurnPolicy() provider.BusyTurnPolicy { return provider.BusyTurnQueue }
