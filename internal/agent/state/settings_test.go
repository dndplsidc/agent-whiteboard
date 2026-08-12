package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

func TestSchema2PersistsCompleteCodexSettingsAndPresentation(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	identity := codexIdentity()
	session := codexSession(t, testID, "sessions/codex-current", now, codexSettings("gpt-5.6-sol", "high", provider.SpeedFast), "5.6 Sol", true)

	outcome, err := store.Create(identity, session, now)
	require.NoError(t, err)
	require.Equal(t, CommitApplied, outcome)
	loaded, err := store.Load(identity)
	require.NoError(t, err)
	require.Equal(t, session.Settings, loaded.Current.Settings)
	require.Equal(t, session.Presentation, loaded.Current.Presentation)

	key, err := ConversationKey(identity)
	require.NoError(t, err)
	encoded, err := os.ReadFile(filepath.Join(store.Root(), "conversations", key+".json"))
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, json.Unmarshal(encoded, &document))
	require.Equal(t, float64(2), document["schema_version"])
	current := document["current"].(map[string]any)
	require.Equal(t, []string{"committed", "conversation_id", "created_at", "model_label", "model_presentation", "native_session_ref", "observed", "prepared_commit", "provider_label", "settings", "updated_at"}, sortedKeys(current))
	require.Equal(t, []string{"effort", "model", "speed"}, sortedKeys(current["settings"].(map[string]any)))
	require.Equal(t, []string{"model_display_name", "selectable"}, sortedKeys(current["model_presentation"].(map[string]any)))
}

func TestSchema2EnforcesProviderSpecificSettingsPresence(t *testing.T) {
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	settings := codexSettings("gpt-5.6-sol", "high", provider.SpeedFast)
	presentation := provider.ModelPresentation{ModelDisplayName: "5.6 Sol", Selectable: true}

	pi := testSession(t, testID, "sessions/pi-current", now)
	pi.Settings = &settings
	pi.Presentation = &presentation
	mapping := Mapping{SchemaVersion: SchemaVersion, Identity: testIdentity(), Current: &pi, Archives: []Session{}, CreatedAt: now, UpdatedAt: now}
	require.Error(t, mapping.Validate(testIdentity()))

	codex := codexSession(t, testID, "sessions/codex-current", now, settings, "5.6 Sol", true)
	codex.Settings = nil
	codexMapping := Mapping{SchemaVersion: SchemaVersion, Identity: codexIdentity(), Current: &codex, Archives: []Session{}, CreatedAt: now, UpdatedAt: now}
	require.Error(t, codexMapping.Validate(codexIdentity()))
	codex = codexSession(t, testID, "sessions/codex-current", now, settings, "5.6 Sol", true)
	codex.Presentation = nil
	codexMapping.Current = &codex
	require.Error(t, codexMapping.Validate(codexIdentity()))

	codex = codexSession(t, testID, "sessions/codex-current", now, settings, "5.6 Sol", true)
	codex.ModelLabel = "different"
	codexMapping.Current = &codex
	require.Error(t, codexMapping.Validate(codexIdentity()))
}

func TestSchema2RejectsSchema1AndMalformedSettingsWithoutMigration(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	identity := codexIdentity()
	_, err := store.Create(identity, codexSession(t, testID, "sessions/codex-current", now, codexSettings("gpt-5.6-sol", "high", provider.SpeedFast), "5.6 Sol", true), now)
	require.NoError(t, err)
	key, err := ConversationKey(identity)
	require.NoError(t, err)
	path := filepath.Join(store.Root(), "conversations", key+".json")
	canonical, err := os.ReadFile(path)
	require.NoError(t, err)

	for name, mutate := range map[string]func(map[string]any){
		"schema 1":         func(document map[string]any) { document["schema_version"] = float64(1) },
		"missing settings": func(document map[string]any) { delete(document["current"].(map[string]any), "settings") },
		"partial settings": func(document map[string]any) {
			delete(document["current"].(map[string]any)["settings"].(map[string]any), "effort")
		},
		"null settings": func(document map[string]any) { document["current"].(map[string]any)["settings"] = nil },
		"unknown settings field": func(document map[string]any) {
			document["current"].(map[string]any)["settings"].(map[string]any)["service_tier"] = "priority"
		},
		"missing presentation": func(document map[string]any) { delete(document["current"].(map[string]any), "model_presentation") },
		"partial presentation": func(document map[string]any) {
			delete(document["current"].(map[string]any)["model_presentation"].(map[string]any), "selectable")
		},
	} {
		t.Run(name, func(t *testing.T) {
			var document map[string]any
			require.NoError(t, json.Unmarshal(canonical, &document))
			mutate(document)
			corrupt, err := json.Marshal(document)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, corrupt, 0o600))
			_, err = store.Load(identity)
			require.Error(t, err)
			require.NoError(t, os.WriteFile(path, canonical, 0o600))
		})
	}
}

func TestUpdateCurrentSettingsRequiresExactCurrentSessionAndIsAtomic(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	identity := codexIdentity()
	initial := codexSession(t, testID, "sessions/codex-current", now, codexSettings("gpt-5.6-sol", "medium", provider.SpeedStandard), "5.6 Sol", true)
	_, err := store.Create(identity, initial, now)
	require.NoError(t, err)

	next := codexSettings("gpt-5.6-luna", "high", provider.SpeedFast)
	presentation := provider.ModelPresentation{ModelDisplayName: "5.6 Luna", Selectable: true}
	outcome, err := store.UpdateCurrentSettings(identity, initial.ConversationID, initial.NativeSession, next, presentation, now.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, CommitApplied, outcome)
	loaded, err := store.Load(identity)
	require.NoError(t, err)
	require.Equal(t, next, *loaded.Current.Settings)
	require.Equal(t, presentation, *loaded.Current.Presentation)
	require.Equal(t, presentation.ModelDisplayName, loaded.Current.ModelLabel)
	require.Equal(t, now.Add(time.Minute), loaded.Current.UpdatedAt)

	wrongRef, err := provider.NewNativeSessionRef("sessions/other")
	require.NoError(t, err)
	outcome, err = store.UpdateCurrentSettings(identity, initial.ConversationID, wrongRef, initialSettings(initial), provider.ModelPresentation{ModelDisplayName: "wrong", Selectable: true}, now.Add(2*time.Minute))
	require.Error(t, err)
	require.Equal(t, CommitNotApplied, outcome)
	outcome, err = store.UpdateCurrentSettings(identity, "MTIzNDU2Nzg5MGFiY2RlZmdoaWprbG1u", initial.NativeSession, initialSettings(initial), provider.ModelPresentation{ModelDisplayName: "wrong", Selectable: true}, now.Add(2*time.Minute))
	require.Error(t, err)
	require.Equal(t, CommitNotApplied, outcome)

	after, err := store.Load(identity)
	require.NoError(t, err)
	require.Equal(t, loaded, after)
}

func TestGenericUpdateCannotMutateSettingsAndArchiveRestoreRetainsThem(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	identity := codexIdentity()
	first := codexSession(t, testID, "sessions/first", now, codexSettings("gpt-5.6-sol", "high", provider.SpeedFast), "5.6 Sol", true)
	_, err := store.Create(identity, first, now)
	require.NoError(t, err)

	outcome, err := store.Update(identity, func(mapping *Mapping) error {
		changed := codexSettings("gpt-5.6-luna", "medium", provider.SpeedStandard)
		mapping.Current.Settings = &changed
		return nil
	})
	require.Error(t, err)
	require.Equal(t, CommitNotApplied, outcome)

	second := codexSession(t, "MTIzNDU2Nzg5MGFiY2RlZmdoaWprbG1u", "sessions/second", now.Add(time.Minute), codexSettings("gpt-5.6-luna", "medium", provider.SpeedStandard), "5.6 Luna", true)
	_, err = store.NewConversation(identity, second, now.Add(time.Minute))
	require.NoError(t, err)
	_, err = store.RestoreArchive(identity, first.ConversationID, now.Add(2*time.Minute))
	require.NoError(t, err)
	loaded, err := store.Load(identity)
	require.NoError(t, err)
	require.Equal(t, *first.Settings, *loaded.Current.Settings)
	require.Equal(t, *first.Presentation, *loaded.Current.Presentation)
	require.Equal(t, *second.Settings, *loaded.Archives[0].Settings)
}

func TestLoadedSettingsAndPresentationAreDeeplyIndependent(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	identity := codexIdentity()
	first := codexSession(t, testID, "sessions/first", now, codexSettings("gpt-5.6-sol", "high", provider.SpeedFast), "5.6 Sol", true)
	second := codexSession(t, "MTIzNDU2Nzg5MGFiY2RlZmdoaWprbG1u", "sessions/second", now.Add(time.Minute), codexSettings("gpt-5.6-luna", "medium", provider.SpeedStandard), "5.6 Luna", true)
	_, err := store.Create(identity, first, now)
	require.NoError(t, err)
	_, err = store.NewConversation(identity, second, now.Add(time.Minute))
	require.NoError(t, err)

	loaded, err := store.Load(identity)
	require.NoError(t, err)
	loaded.Current.Settings.Model = "mutated"
	loaded.Current.Presentation.ModelDisplayName = "mutated"
	require.Equal(t, "gpt-5.6-sol", loaded.Archives[0].Settings.Model)
	require.Equal(t, "5.6 Sol", loaded.Archives[0].Presentation.ModelDisplayName)
	reloaded, err := store.Load(identity)
	require.NoError(t, err)
	require.Equal(t, "gpt-5.6-luna", reloaded.Current.Settings.Model)
	require.Equal(t, "5.6 Luna", reloaded.Current.Presentation.ModelDisplayName)
}

func codexIdentity() Identity {
	identity := testIdentity()
	identity.Provider = provider.NameCodex
	return identity
}

func codexSettings(model, effort string, speed provider.ExecutionSpeed) provider.ExecutionSettings {
	return provider.ExecutionSettings{Model: model, Effort: effort, Speed: speed}
}

func codexSession(t *testing.T, id, native string, at time.Time, settings provider.ExecutionSettings, displayName string, selectable bool) Session {
	t.Helper()
	ref, err := provider.NewNativeSessionRef(native)
	require.NoError(t, err)
	presentation := provider.ModelPresentation{ModelDisplayName: displayName, Selectable: selectable}
	return Session{
		ConversationID: id, NativeSession: ref, CreatedAt: at, UpdatedAt: at,
		ProviderLabel: "Codex", ModelLabel: displayName, Settings: &settings, Presentation: &presentation,
	}
}

func initialSettings(session Session) provider.ExecutionSettings {
	return *session.Settings
}
