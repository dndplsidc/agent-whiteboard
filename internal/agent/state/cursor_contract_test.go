package state

import (
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

func TestSchema2CursorMappingsRequireCompleteSettingsAndRoundTrip(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC)
	identity := testIdentity()
	identity.Provider = provider.NameCursor
	settings := provider.ExecutionSettings{Model: "cursor/model-exact", Effort: "default", Speed: provider.SpeedStandard}
	presentation := provider.ModelPresentation{ModelDisplayName: "Cursor Model", Selectable: true}
	session := testSession(t, testID, "sessions/cursor-current", now)
	session.ProviderLabel = "Cursor"
	session.ModelLabel = presentation.ModelDisplayName
	session.Settings = &settings
	session.Presentation = &presentation

	outcome, err := store.Create(identity, session, now)
	require.NoError(t, err)
	require.Equal(t, CommitApplied, outcome)
	loaded, err := store.Load(identity)
	require.NoError(t, err)
	require.Equal(t, SchemaVersion, loaded.SchemaVersion)
	require.Equal(t, identity, loaded.Identity)
	require.Equal(t, settings, *loaded.Current.Settings)
	require.Equal(t, presentation, *loaded.Current.Presentation)

	missingSettings := session
	missingSettings.Settings = nil
	missingSettings.Presentation = nil
	mapping := Mapping{SchemaVersion: SchemaVersion, Identity: identity, Current: &missingSettings, Archives: []Session{}, CreatedAt: now, UpdatedAt: now}
	require.Error(t, mapping.Validate(identity))
}
