package provider_test

import (
	"context"
	"testing"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

func TestProviderIdentityDeletionAndCleanupContracts(t *testing.T) {
	require.Equal(t, []provider.Name{provider.NamePi, provider.NameCodex, provider.NameCursor}, provider.AllNames())
	require.True(t, provider.NameCursor.Valid())
	require.False(t, provider.Name("Cursor").Valid())

	registry, err := provider.NewRegistry(map[provider.Name]provider.Driver{
		provider.NameCursor: &fakeDriver{},
		provider.NameCodex:  &fakeDriver{},
		provider.NamePi:     &fakeDriver{},
	})
	require.NoError(t, err)
	require.Equal(t, []provider.Name{provider.NamePi, provider.NameCodex, provider.NameCursor}, registry.Names())

	var driver provider.Driver = &driverWithoutDelete{}
	_, supportsDelete := driver.(provider.NativeSessionDeleter)
	require.False(t, supportsDelete)
	_, supportsDelete = any(&fakeDriver{}).(provider.NativeSessionDeleter)
	require.True(t, supportsDelete)

	for _, policy := range []provider.CleanupPolicy{
		{NativeDeletion: provider.NativeDeletionRequired, RemoveWorkspace: true},
		{NativeDeletion: provider.NativeDeletionUnsupported, RemoveWorkspace: true},
		{NativeDeletion: provider.NativeDeletionUnsafe, RemoveWorkspace: false},
	} {
		require.NoError(t, policy.Validate())
	}
	for _, policy := range []provider.CleanupPolicy{
		{NativeDeletion: provider.NativeDeletionRequired, RemoveWorkspace: false},
		{NativeDeletion: provider.NativeDeletionUnsupported, RemoveWorkspace: false},
		{NativeDeletion: provider.NativeDeletionUnsafe, RemoveWorkspace: true},
		{NativeDeletion: "unknown"},
	} {
		require.Error(t, policy.Validate())
	}

	unsupported := provider.NewProviderError(provider.ErrorArchiveDeleteUnsupported)
	require.True(t, unsupported.Valid())
	require.Contains(t, provider.AllProviderErrorCodes(), provider.ErrorArchiveDeleteUnsupported)
}

type driverWithoutDelete struct{}

func (*driverWithoutDelete) Readiness(context.Context) provider.Readiness {
	return provider.Readiness{}
}
func (*driverWithoutDelete) Create(context.Context, provider.CreateRequest) (provider.Session, error) {
	return nil, nil
}
func (*driverWithoutDelete) Resume(context.Context, provider.ResumeRequest) (provider.Session, error) {
	return nil, nil
}
func (*driverWithoutDelete) Inspect(context.Context, provider.InspectRequest) (provider.NativeSession, error) {
	return provider.NativeSession{}, nil
}

func TestPreflightCapacityModesAreClosedAndCrossFieldValidated(t *testing.T) {
	prechecked := provider.PreflightResult{
		CapacityMode: provider.CapacityPrechecked, ResolvedModel: "model",
		EstimatedInputTokens: 100, EffectiveCapacityTokens: 200, SafetyMarginTokens: 50,
	}
	require.NoError(t, prechecked.Validate())

	providerEnforced := provider.PreflightResult{CapacityMode: provider.CapacityProviderEnforced, ResolvedModel: "model"}
	require.NoError(t, providerEnforced.Validate())

	for _, invalid := range []provider.PreflightResult{
		{CapacityMode: "other", ResolvedModel: "model"},
		{CapacityMode: provider.CapacityPrechecked, ResolvedModel: "model"},
		{CapacityMode: provider.CapacityProviderEnforced, ResolvedModel: "model", EstimatedInputTokens: 1},
		{CapacityMode: provider.CapacityProviderEnforced, ResolvedModel: "model", EffectiveCapacityTokens: 1},
		{CapacityMode: provider.CapacityProviderEnforced, ResolvedModel: "model", SafetyMarginTokens: 1},
	} {
		require.Error(t, invalid.Validate())
	}
}

func TestCursorSettingsProjectionUsesExactSharedValues(t *testing.T) {
	catalog := provider.ModelCatalog{Models: []provider.CatalogModel{{
		Model: "cursor/model-exact", DisplayName: "Cursor Display Name", DefaultEffort: "default",
		SupportedReasoningEfforts: []provider.ReasoningEffort{{Value: "default"}},
		SupportsImages:            true, Default: true, SupportsFast: false,
	}}}
	require.NoError(t, catalog.Validate())
	settings := provider.ExecutionSettings{Model: "cursor/model-exact", Effort: "default", Speed: provider.SpeedStandard}
	canonical, err := catalog.Canonicalize(settings)
	require.NoError(t, err)
	require.Equal(t, settings, canonical)
	model, err := catalog.Resolve(settings)
	require.NoError(t, err)
	require.Equal(t, "Cursor Display Name", model.DisplayName)
	require.False(t, catalog.Compatibility(provider.ExecutionSettings{Model: settings.Model, Effort: settings.Effort, Speed: provider.SpeedFast}).Compatible)
}
