package provider_test

import (
	"strings"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

func TestExecutionSettingsCatalogValidationCanonicalizationAndCloneSafety(t *testing.T) {
	catalog := validModelCatalog()
	require.NoError(t, catalog.Validate())

	settings := provider.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "high", Speed: provider.SpeedFast}
	require.NoError(t, settings.Validate())
	canonical, err := catalog.Canonicalize(settings)
	require.NoError(t, err)
	require.Equal(t, settings, canonical)
	model, err := catalog.Resolve(settings)
	require.NoError(t, err)
	require.Equal(t, "5.6 Sol", model.DisplayName)

	compatibility := catalog.Compatibility(settings)
	require.True(t, compatibility.Compatible)
	require.Empty(t, compatibility.Reason)

	clone := catalog.Clone()
	clone.Models[0].DisplayName = "changed"
	clone.Models[0].SupportedReasoningEfforts[0].Description = "changed"
	require.Equal(t, "5.6 Sol", catalog.Models[0].DisplayName)
	require.Equal(t, "Quick", catalog.Models[0].SupportedReasoningEfforts[0].Description)
}

func TestExecutionSettingsCatalogRejectsInvalidAndIncompatibleValues(t *testing.T) {
	catalog := validModelCatalog()

	cases := []struct {
		name   string
		change func(*provider.ModelCatalog)
	}{
		{"empty models", func(value *provider.ModelCatalog) { value.Models = nil }},
		{"duplicate model", func(value *provider.ModelCatalog) { value.Models = append(value.Models, value.Models[0]) }},
		{"duplicate effort", func(value *provider.ModelCatalog) {
			value.Models[0].SupportedReasoningEfforts = append(value.Models[0].SupportedReasoningEfforts, value.Models[0].SupportedReasoningEfforts[0])
		}},
		{"missing default effort", func(value *provider.ModelCatalog) { value.Models[0].DefaultEffort = "xhigh" }},
		{"multiple defaults", func(value *provider.ModelCatalog) { value.Models[1].Default = true }},
		{"too many models", func(value *provider.ModelCatalog) {
			value.Models = make([]provider.CatalogModel, provider.MaxCatalogModels+1)
		}},
		{"too many efforts", func(value *provider.ModelCatalog) {
			value.Models[0].SupportedReasoningEfforts = make([]provider.ReasoningEffort, provider.MaxReasoningEfforts+1)
		}},
		{"aggregate overflow", func(value *provider.ModelCatalog) {
			value.Models[0].Description = strings.Repeat("x", provider.MaxCatalogBytes+1)
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			changed := catalog.Clone()
			test.change(&changed)
			require.Error(t, changed.Validate())
		})
	}

	unknown := catalog.Compatibility(provider.ExecutionSettings{Model: "missing", Effort: "high", Speed: provider.SpeedStandard})
	require.False(t, unknown.Compatible)
	require.Equal(t, provider.IncompatibleModelUnavailable, unknown.Reason)
	unsupportedEffort := catalog.Compatibility(provider.ExecutionSettings{Model: "gpt-5.6-luna", Effort: "high", Speed: provider.SpeedStandard})
	require.False(t, unsupportedEffort.Compatible)
	require.Equal(t, provider.IncompatibleEffortUnsupported, unsupportedEffort.Reason)
	unsupportedFast := catalog.Compatibility(provider.ExecutionSettings{Model: "gpt-5.6-luna", Effort: "medium", Speed: provider.SpeedFast})
	require.False(t, unsupportedFast.Compatible)
	require.Equal(t, provider.IncompatibleFastUnsupported, unsupportedFast.Reason)

	_, err := catalog.Canonicalize(provider.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "xhigh", Speed: provider.SpeedFast})
	require.Error(t, err)
	require.Error(t, (provider.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "high", Speed: "priority"}).Validate())
}

func TestCompleteSettingsAreProviderNeutralAcrossLifecycleContracts(t *testing.T) {
	workspace := t.TempDir()
	settings := provider.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "high", Speed: provider.SpeedFast}
	presentation := provider.ModelPresentation{ModelDisplayName: "5.6 Sol", Selectable: true}

	require.NoError(t, (provider.CreateRequest{Provider: provider.NamePi, Access: provider.AccessConfigured, Workspace: workspace}).Validate())
	require.NoError(t, (provider.CreateRequest{Provider: provider.NamePi, Access: provider.AccessConfigured, Workspace: workspace, Settings: &settings}).Validate())
	require.NoError(t, (provider.CreateRequest{Provider: provider.NameCodex, Access: provider.AccessConfigured, Workspace: workspace}).Validate())
	require.NoError(t, (provider.CreateRequest{Provider: provider.NameCodex, Access: provider.AccessConfigured, Workspace: workspace, Settings: &settings}).Validate())

	turn := validTurnRequest()
	turn.Settings = &settings
	require.NoError(t, turn.Validate())
	partial := settings
	partial.Effort = ""
	turn.Settings = &partial
	require.Error(t, turn.Validate())

	ref, err := provider.NewNativeSessionRef("codex-session-reference")
	require.NoError(t, err)
	now := time.Now().UTC()
	codex := provider.NativeSession{Ref: ref, Provider: provider.NameCodex, Model: settings.Model, Settings: &settings, Presentation: &presentation, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, codex.Validate())
	codex.Settings = nil
	require.Error(t, codex.Validate())

	pi := validNativeSession()
	pi.Model = settings.Model
	pi.Settings = &settings
	pi.Presentation = &presentation
	require.NoError(t, pi.Validate())
	pi.Settings = nil
	require.Error(t, pi.Validate(), "legacy nil settings are accepted only by durable state validation")

	accepted := provider.AcceptedTurn{TurnID: idA, AcceptedAt: now, Settings: &settings, Presentation: &presentation}
	require.NoError(t, accepted.Validate())
	accepted.Presentation = nil
	require.Error(t, accepted.Validate())
}

func TestSettingsEventsRemainOptionalAndComplete(t *testing.T) {
	settings := provider.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "high", Speed: provider.SpeedFast}
	presentation := provider.ModelPresentation{ModelDisplayName: "5.6 Sol", Selectable: true}
	verified := provider.NewVerifiedSettingsEvent(idA, settings, presentation)
	require.NoError(t, verified.Validate())
	require.Equal(t, provider.EventSettings, verified.Kind)
	require.Equal(t, provider.SettingsVerified, verified.SettingsState)
	unverified := provider.NewUnverifiedSettingsEvent(idA)
	require.NoError(t, unverified.Validate())
	require.Equal(t, provider.SettingsUnverified, unverified.SettingsState)

	invalid := verified
	invalid.Presentation = nil
	require.Error(t, invalid.Validate())
	invalid = unverified
	invalid.Settings = &settings
	require.Error(t, invalid.Validate())
}

func TestInvalidModelConfigurationErrorIsClosedAndRedacted(t *testing.T) {
	err := provider.NewProviderError(provider.ErrorInvalidModelConfiguration)
	require.True(t, err.Valid())
	require.Equal(t, provider.ErrorInvalidModelConfiguration, err.Code())
	require.NotContains(t, err.Error(), "/")
	require.Contains(t, provider.AllProviderErrorCodes(), provider.ErrorInvalidModelConfiguration)
}

func validModelCatalog() provider.ModelCatalog {
	return provider.ModelCatalog{Models: []provider.CatalogModel{
		{
			Model: "gpt-5.6-sol", DisplayName: "5.6 Sol", Description: "Deep coding work", DefaultEffort: "medium",
			SupportedReasoningEfforts: []provider.ReasoningEffort{{Value: "low", Description: "Quick"}, {Value: "medium", Description: "Balanced"}, {Value: "high", Description: "Deep"}},
			SupportsImages:            true, Default: true, SupportsFast: true,
		},
		{
			Model: "gpt-5.6-luna", DisplayName: "5.6 Luna", Description: "Plan execution", DefaultEffort: "medium",
			SupportedReasoningEfforts: []provider.ReasoningEffort{{Value: "medium", Description: "Balanced"}},
			SupportsImages:            false,
		},
	}}
}
