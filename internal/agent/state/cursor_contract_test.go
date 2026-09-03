package state

import (
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

func TestReplaceMissingCursorNativeSessionPreservesPromptFreeMapping(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC)
	identity := testIdentity()
	identity.Provider = provider.NameCursor
	settings := provider.ExecutionSettings{Model: "cursor/model-exact", Effort: "default", Speed: provider.SpeedStandard}
	presentation := provider.ModelPresentation{ModelDisplayName: "Cursor Model", Selectable: true}
	session := testSession(t, testID, "sessions/cursor-missing", now)
	session.ProviderLabel = "Cursor"
	session.ModelLabel = presentation.ModelDisplayName
	session.Settings = &settings
	session.Presentation = &presentation
	observed := Revision{Digest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Revision: RevisionInitial, SourceUpdatedAt: now}
	session.Observed = &observed
	archived := session
	archived.ConversationID = "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd5"
	archivedRef, err := provider.NewNativeSessionRef("sessions/cursor-archived")
	require.NoError(t, err)
	archived.NativeSession = archivedRef
	_, err = store.Create(identity, archived, now)
	require.NoError(t, err)
	_, err = store.NewConversation(identity, session, now.Add(time.Second))
	require.NoError(t, err)
	expected, err := store.Load(identity)
	require.NoError(t, err)
	require.Len(t, expected.Archives, 1)
	require.Equal(t, archived, expected.Archives[0])
	replacement, err := provider.NewNativeSessionRef("sessions/cursor-replacement")
	require.NoError(t, err)

	outcome, err := store.Update(identity, func(mapping *Mapping) error {
		mapping.Current.NativeSession = replacement
		return nil
	})
	require.Error(t, err)
	require.Equal(t, CommitNotApplied, outcome)

	outcome, err = store.ReplaceMissingCurrentNativeSessionIfUnchanged(identity, expected, replacement, now.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, CommitApplied, outcome)
	loaded, err := store.Load(identity)
	require.NoError(t, err)
	require.Equal(t, replacement, loaded.Current.NativeSession)
	require.Equal(t, expected.Current.ConversationID, loaded.Current.ConversationID)
	require.Equal(t, expected.Current.CreatedAt, loaded.Current.CreatedAt)
	require.Equal(t, expected.Current.Observed, loaded.Current.Observed)
	require.Equal(t, expected.Current.Settings, loaded.Current.Settings)
	require.Equal(t, expected.Current.Presentation, loaded.Current.Presentation)
	require.Equal(t, expected.Archives, loaded.Archives)

	outcome, err = store.ReplaceMissingCurrentNativeSessionIfUnchanged(identity, expected, replacement, now.Add(2*time.Minute))
	require.Error(t, err)
	require.Equal(t, CommitNotApplied, outcome)
}

func TestReplacePromptFreeCursorNativeSessionAndSettingsAtomically(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC)
	identity := testIdentity()
	identity.Provider = provider.NameCursor
	oldSettings := provider.ExecutionSettings{Model: "cursor-old", Effort: "default", Speed: provider.SpeedStandard}
	oldPresentation := provider.ModelPresentation{ModelDisplayName: "Cursor Old", Selectable: true}
	session := testSession(t, testID, "sessions/cursor-missing", now)
	session.ProviderLabel = "Cursor"
	session.ModelLabel = oldPresentation.ModelDisplayName
	session.Settings = &oldSettings
	session.Presentation = &oldPresentation
	observed := Revision{Digest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Revision: RevisionInitial, SourceUpdatedAt: now}
	session.Observed = &observed
	_, err := store.Create(identity, session, now)
	require.NoError(t, err)
	expected, err := store.Load(identity)
	require.NoError(t, err)
	replacement, err := provider.NewNativeSessionRef("sessions/cursor-replacement")
	require.NoError(t, err)
	newSettings := provider.ExecutionSettings{Model: "cursor-new", Effort: "default", Speed: provider.SpeedStandard}
	newPresentation := provider.ModelPresentation{ModelDisplayName: "Cursor New", Selectable: true}

	outcome, err := store.ReplacePromptFreeCurrentNativeSessionAndSettingsIfUnchanged(identity, expected, replacement, newSettings, newPresentation, now.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, CommitApplied, outcome)
	loaded, err := store.Load(identity)
	require.NoError(t, err)
	require.Equal(t, replacement, loaded.Current.NativeSession)
	require.Equal(t, newSettings, *loaded.Current.Settings)
	require.Equal(t, newPresentation, *loaded.Current.Presentation)
	require.Equal(t, newPresentation.ModelDisplayName, loaded.Current.ModelLabel)
	require.Equal(t, expected.Current.ConversationID, loaded.Current.ConversationID)
	require.Equal(t, expected.Current.CreatedAt, loaded.Current.CreatedAt)
	require.Equal(t, expected.Current.Observed, loaded.Current.Observed)

	outcome, err = store.ReplacePromptFreeCurrentNativeSessionAndSettingsIfUnchanged(identity, expected, replacement, newSettings, newPresentation, now.Add(2*time.Minute))
	require.Error(t, err)
	require.Equal(t, CommitNotApplied, outcome)
}

func TestReplaceMissingCursorNativeSessionRejectsChangedMapping(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC)
	identity := testIdentity()
	identity.Provider = provider.NameCursor
	settings := provider.ExecutionSettings{Model: "cursor/model-exact", Effort: "default", Speed: provider.SpeedStandard}
	presentation := provider.ModelPresentation{ModelDisplayName: "Cursor Model", Selectable: true}
	session := testSession(t, testID, "sessions/cursor-missing", now)
	session.ProviderLabel = "Cursor"
	session.ModelLabel = presentation.ModelDisplayName
	session.Settings = &settings
	session.Presentation = &presentation
	_, err := store.Create(identity, session, now)
	require.NoError(t, err)
	expected, err := store.Load(identity)
	require.NoError(t, err)
	observed := Revision{Digest: "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", Revision: RevisionReplacement, SourceUpdatedAt: now.Add(time.Minute)}
	outcome, err := store.ObserveRevision(identity, observed, now.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, CommitApplied, outcome)
	replacement, err := provider.NewNativeSessionRef("sessions/cursor-replacement")
	require.NoError(t, err)

	outcome, err = store.ReplaceMissingCurrentNativeSessionIfUnchanged(identity, expected, replacement, now.Add(2*time.Minute))
	require.Error(t, err)
	require.Equal(t, CommitNotApplied, outcome)
	loaded, err := store.Load(identity)
	require.NoError(t, err)
	require.Equal(t, expected.Current.NativeSession, loaded.Current.NativeSession)
	require.Equal(t, &observed, loaded.Current.Observed)
}

func TestReplaceMissingCursorNativeSessionRejectsCommittedMapping(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC)
	identity := testIdentity()
	identity.Provider = provider.NameCursor
	settings := provider.ExecutionSettings{Model: "cursor/model-exact", Effort: "default", Speed: provider.SpeedStandard}
	presentation := provider.ModelPresentation{ModelDisplayName: "Cursor Model", Selectable: true}
	session := testSession(t, testID, "sessions/cursor-missing", now)
	session.ProviderLabel = "Cursor"
	session.ModelLabel = presentation.ModelDisplayName
	session.Settings = &settings
	session.Presentation = &presentation
	committed := Revision{Digest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Revision: RevisionInitial, SourceUpdatedAt: now}
	session.Committed = &committed
	_, err := store.Create(identity, session, now)
	require.NoError(t, err)
	expected, err := store.Load(identity)
	require.NoError(t, err)
	replacement, err := provider.NewNativeSessionRef("sessions/cursor-replacement")
	require.NoError(t, err)

	outcome, err := store.ReplaceMissingCurrentNativeSessionIfUnchanged(identity, expected, replacement, now.Add(time.Minute))
	require.Error(t, err)
	require.Equal(t, CommitNotApplied, outcome)
}

func TestReplaceMissingCursorNativeSessionRejectsPreparedMapping(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC)
	identity := testIdentity()
	identity.Provider = provider.NameCursor
	settings := provider.ExecutionSettings{Model: "cursor/model-exact", Effort: "default", Speed: provider.SpeedStandard}
	presentation := provider.ModelPresentation{ModelDisplayName: "Cursor Model", Selectable: true}
	session := testSession(t, testID, "sessions/cursor-missing", now)
	session.ProviderLabel = "Cursor"
	session.ModelLabel = presentation.ModelDisplayName
	session.Settings = &settings
	session.Presentation = &presentation
	observed := Revision{Digest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Revision: RevisionInitial, SourceUpdatedAt: now}
	session.Observed = &observed
	session.PreparedCommit = &PreparedCommit{Revision: observed, TurnID: testID, Phase: CommitPrepared}
	_, err := store.Create(identity, session, now)
	require.NoError(t, err)
	expected, err := store.Load(identity)
	require.NoError(t, err)
	replacement, err := provider.NewNativeSessionRef("sessions/cursor-replacement")
	require.NoError(t, err)

	outcome, err := store.ReplaceMissingCurrentNativeSessionIfUnchanged(identity, expected, replacement, now.Add(time.Minute))
	require.Error(t, err)
	require.Equal(t, CommitNotApplied, outcome)
	loaded, err := store.Load(identity)
	require.NoError(t, err)
	require.Equal(t, expected, loaded)
}

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
