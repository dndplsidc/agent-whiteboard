package state

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

const testID = "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4"

func testIdentity() Identity {
	return Identity{Origin: "https://example.test", Kind: ResourceMarkdown, CapabilityID: testID, Provider: provider.NamePi}
}

func testSession(t *testing.T, id, native string, at time.Time) Session {
	t.Helper()
	ref, err := provider.NewNativeSessionRef(native)
	require.NoError(t, err)
	return Session{ConversationID: id, NativeSession: ref, CreatedAt: at, UpdatedAt: at, ProviderLabel: "Pi", ModelLabel: "model"}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func TestConversationKeyCanonicalVector(t *testing.T) {
	identity := testIdentity()
	key, err := ConversationKey(identity)
	require.NoError(t, err)

	encoded := []byte("agent-whiteboard-conversation-key-v1")
	for _, value := range []string{identity.Origin, string(identity.Kind), identity.CapabilityID, string(identity.Provider)} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		encoded = append(encoded, length[:]...)
		encoded = append(encoded, value...)
	}
	require.Equal(t, independentSHA256Hex(encoded), key)
	require.Equal(t, "8a0ea647502ce21ca9f4ed43ded38690edd8730b814733efd637fbe14f00a195", key)

	identity.Origin = "https://EXAMPLE.test"
	_, err = ConversationKey(identity)
	require.Error(t, err, "the key requires the exact canonical origin")

	identity.Origin = "http://127.0.0.1:8080"
	_, err = ConversationKey(identity)
	require.NoError(t, err, "literal loopback HTTP is a valid durable conversation origin")
	identity.Origin = "HTTP://127.0.0.1:8080"
	_, err = ConversationKey(identity)
	require.Error(t, err, "the loopback origin must also be exactly canonical")
}

func TestHTMLIdentityPersistsSeparatelyFromMarkdown(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	html := testIdentity()
	html.Kind = ResourceHTML
	markdownKey, err := ConversationKey(testIdentity())
	require.NoError(t, err)
	htmlKey, err := ConversationKey(html)
	require.NoError(t, err)
	require.NotEqual(t, markdownKey, htmlKey)

	current := testSession(t, testID, "sessions/html.jsonl", now)
	outcome, err := store.Create(html, current, now)
	require.NoError(t, err)
	require.Equal(t, CommitApplied, outcome)
	loaded, err := store.Load(html)
	require.NoError(t, err)
	require.Equal(t, ResourceHTML, loaded.Identity.Kind)
	_, err = store.Load(testIdentity())
	require.Error(t, err)
}

func TestCreateLoadAndStrictDurableSchema(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	current := testSession(t, testID, "sessions/current.jsonl", now)

	outcome, err := store.Create(testIdentity(), current, now)
	require.NoError(t, err)
	require.Equal(t, CommitApplied, outcome)

	loaded, err := store.Load(testIdentity())
	require.NoError(t, err)
	require.Equal(t, current.NativeSession.Value(), loaded.Current.NativeSession.Value())

	key, err := ConversationKey(testIdentity())
	require.NoError(t, err)
	durable, err := os.ReadFile(filepath.Join(store.Root(), "conversations", key+".json"))
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, json.Unmarshal(durable, &document))
	require.Equal(t, []string{"archives", "created_at", "current", "identity", "schema_version", "updated_at"}, sortedKeys(document))
	require.Equal(t, []string{"capability_id", "kind", "origin", "provider"}, sortedKeys(document["identity"].(map[string]any)))
	require.Equal(t, []string{"committed", "conversation_id", "created_at", "model_label", "model_presentation", "native_session_ref", "observed", "prepared_commit", "provider_label", "settings", "updated_at"}, sortedKeys(document["current"].(map[string]any)))
}

func TestRevisionPreparePromoteAndReconcile(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	session := testSession(t, testID, "sessions/current", now)
	_, err := store.Create(testIdentity(), session, now)
	require.NoError(t, err)

	revision := Revision{Digest: strings.Repeat("a", 64), Revision: RevisionInitial, SourceUpdatedAt: now}
	outcome, err := store.PrepareCommit(testIdentity(), revision, testID, now.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, CommitApplied, outcome)
	key, err := ConversationKey(testIdentity())
	require.NoError(t, err)
	encoded, err := os.ReadFile(filepath.Join(store.Root(), "conversations", key+".json"))
	require.NoError(t, err)
	var preparedDocument map[string]any
	require.NoError(t, json.Unmarshal(encoded, &preparedDocument))
	currentDocument := preparedDocument["current"].(map[string]any)
	require.Equal(t, []string{"digest", "phase", "revision", "source_updated_at", "turn_id"}, sortedKeys(currentDocument["prepared_commit"].(map[string]any)))
	require.Equal(t, []string{"digest", "revision", "source_updated_at"}, sortedKeys(currentDocument["observed"].(map[string]any)))

	_, err = store.MarkPreparedAccepted(testIdentity(), testID, now.Add(2*time.Second))
	require.NoError(t, err)
	outcome, err = store.PromotePrepared(testIdentity(), testID, now.Add(3*time.Second))
	require.NoError(t, err)
	require.Equal(t, CommitApplied, outcome)
	loaded, err := store.Load(testIdentity())
	require.NoError(t, err)
	require.Equal(t, revision.Digest, loaded.Current.Committed.Digest)
	require.Nil(t, loaded.Current.Observed)
	require.Nil(t, loaded.Current.PreparedCommit)

	replacement := Revision{Digest: strings.Repeat("b", 64), Revision: RevisionReplacement, SourceUpdatedAt: now.Add(time.Second)}
	_, err = store.PrepareCommit(testIdentity(), replacement, testID, now.Add(4*time.Second))
	require.NoError(t, err)
	_, err = store.MarkPreparedAccepted(testIdentity(), testID, now.Add(5*time.Second))
	require.NoError(t, err)
	loaded, err = store.Load(testIdentity())
	require.NoError(t, err)
	require.Equal(t, CommitAccepted, loaded.Current.PreparedCommit.Phase)
	outcome, err = store.ReconcilePrepared(testIdentity(), testID, false, now.Add(6*time.Second))
	require.Error(t, err)
	require.Equal(t, CommitNotApplied, outcome)
	loaded, err = store.Load(testIdentity())
	require.NoError(t, err)
	require.Equal(t, CommitAccepted, loaded.Current.PreparedCommit.Phase)

	outcome, err = store.PromotePrepared(testIdentity(), testID, now.Add(7*time.Second))
	require.NoError(t, err)
	require.Equal(t, CommitApplied, outcome)
	loaded, err = store.Load(testIdentity())
	require.NoError(t, err)
	require.Nil(t, loaded.Current.PreparedCommit)
	require.Nil(t, loaded.Current.Observed)
	require.Equal(t, replacement, *loaded.Current.Committed)
}

func TestAcknowledgeCommittedRevisionClearsOnlyANewerConflictingObservation(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	session := testSession(t, testID, "sessions/current", now)
	_, err := store.Create(testIdentity(), session, now)
	require.NoError(t, err)
	committed := Revision{Digest: strings.Repeat("a", 64), Revision: RevisionInitial, SourceUpdatedAt: now}
	_, err = store.ObserveRevision(testIdentity(), committed, now)
	require.NoError(t, err)
	_, err = store.PrepareCommit(testIdentity(), committed, testID, now)
	require.NoError(t, err)
	_, err = store.MarkPreparedAccepted(testIdentity(), testID, now)
	require.NoError(t, err)
	_, err = store.PromotePrepared(testIdentity(), testID, now)
	require.NoError(t, err)

	pending := Revision{Digest: strings.Repeat("b", 64), Revision: RevisionReplacement, SourceUpdatedAt: now.Add(time.Second)}
	_, err = store.ObserveRevision(testIdentity(), pending, now.Add(time.Second))
	require.NoError(t, err)
	reverted := Revision{Digest: committed.Digest, Revision: RevisionReplacement, SourceUpdatedAt: now.Add(2 * time.Second)}
	outcome, err := store.AcknowledgeCommittedRevision(testIdentity(), reverted, now.Add(2*time.Second))
	require.NoError(t, err)
	require.Equal(t, CommitApplied, outcome)
	loaded, err := store.Load(testIdentity())
	require.NoError(t, err)
	require.Equal(t, reverted, *loaded.Current.Committed)
	require.Nil(t, loaded.Current.Observed)

	_, err = store.ObserveRevision(testIdentity(), pending, now.Add(3*time.Second))
	require.NoError(t, err)
	outcome, err = store.AcknowledgeCommittedRevision(testIdentity(), Revision{Digest: committed.Digest, Revision: RevisionReplacement, SourceUpdatedAt: pending.SourceUpdatedAt}, now.Add(4*time.Second))
	require.Error(t, err)
	require.Equal(t, CommitNotApplied, outcome)
	loaded, err = store.Load(testIdentity())
	require.NoError(t, err)
	require.Equal(t, pending, *loaded.Current.Observed)
}

func TestPreparedCommitTransitionTable(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	revision := Revision{Digest: strings.Repeat("a", 64), Revision: RevisionInitial, SourceUpdatedAt: now}
	other := Revision{Digest: strings.Repeat("b", 64), Revision: RevisionReplacement, SourceUpdatedAt: now.Add(time.Second)}
	_, err := store.Create(testIdentity(), testSession(t, testID, "sessions/current", now), now)
	require.NoError(t, err)
	_, err = store.PrepareCommit(testIdentity(), revision, testID, now.Add(time.Second))
	require.NoError(t, err)

	outcome, err := store.PrepareCommit(testIdentity(), other, "MTIzNDU2Nzg5MGFiY2RlZmdoaWprbG1u", now.Add(2*time.Second))
	require.Error(t, err)
	require.Equal(t, CommitNotApplied, outcome)
	outcome, err = store.PromotePrepared(testIdentity(), testID, now.Add(3*time.Second))
	require.Error(t, err)
	require.Equal(t, CommitNotApplied, outcome)

	_, err = store.MarkPreparedAccepted(testIdentity(), testID, now.Add(4*time.Second))
	require.NoError(t, err)
	_, err = store.MarkPreparedAccepted(testIdentity(), testID, now.Add(5*time.Second))
	require.NoError(t, err, "acceptance is idempotent for the same turn")
	outcome, err = store.PrepareCommit(testIdentity(), revision, testID, now.Add(6*time.Second))
	require.Error(t, err, "prepare must not downgrade accepted to prepared")
	require.Equal(t, CommitNotApplied, outcome)
	outcome, err = store.ReconcilePrepared(testIdentity(), testID, false, now.Add(7*time.Second))
	require.Error(t, err, "provider rejection cannot discard recorded acceptance")
	require.Equal(t, CommitNotApplied, outcome)

	_, err = store.PromotePrepared(testIdentity(), testID, now.Add(8*time.Second))
	require.NoError(t, err)
	outcome, err = store.PrepareCommit(testIdentity(), revision, testID, now.Add(9*time.Second))
	require.Error(t, err, "a committed revision cannot be prepared for reinjection")
	require.Equal(t, CommitNotApplied, outcome)
}

func TestGenericUpdateCannotOverwriteUnresolvedOrCommittedRevision(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	revision := Revision{Digest: strings.Repeat("a", 64), Revision: RevisionInitial, SourceUpdatedAt: now}
	other := Revision{Digest: strings.Repeat("b", 64), Revision: RevisionReplacement, SourceUpdatedAt: now.Add(time.Second)}
	_, err := store.Create(testIdentity(), testSession(t, testID, "sessions/current", now), now)
	require.NoError(t, err)
	outcome, err := store.Update(testIdentity(), func(mapping *Mapping) error {
		mapping.Current.Committed = &other
		return nil
	})
	require.Error(t, err)
	require.Equal(t, CommitNotApplied, outcome)

	_, err = store.PrepareCommit(testIdentity(), revision, testID, now.Add(time.Second))
	require.NoError(t, err)
	outcome, err = store.Update(testIdentity(), func(mapping *Mapping) error {
		mapping.Current.PreparedCommit.Revision = other
		return nil
	})
	require.Error(t, err)
	require.Equal(t, CommitNotApplied, outcome)
	_, err = store.MarkPreparedAccepted(testIdentity(), testID, now.Add(2*time.Second))
	require.NoError(t, err)
	_, err = store.PromotePrepared(testIdentity(), testID, now.Add(3*time.Second))
	require.NoError(t, err)

	outcome, err = store.Update(testIdentity(), func(mapping *Mapping) error {
		mapping.Current.Committed = nil
		return nil
	})
	require.Error(t, err)
	require.Equal(t, CommitNotApplied, outcome)
}

func TestReconcileProviderProofMayPromotePrepared(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	revision := Revision{Digest: strings.Repeat("a", 64), Revision: RevisionInitial, SourceUpdatedAt: now}
	_, err := store.Create(testIdentity(), testSession(t, testID, "sessions/current", now), now)
	require.NoError(t, err)
	_, err = store.PrepareCommit(testIdentity(), revision, testID, now.Add(time.Second))
	require.NoError(t, err)
	_, err = store.ReconcilePrepared(testIdentity(), testID, true, now.Add(2*time.Second))
	require.NoError(t, err)
	loaded, err := store.Load(testIdentity())
	require.NoError(t, err)
	require.Equal(t, revision, *loaded.Current.Committed)
	require.Nil(t, loaded.Current.PreparedCommit)
}

func TestArchiveTransitionsAndDeletion(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	first := testSession(t, testID, "sessions/first", now)
	secondID := "MTIzNDU2Nzg5MGFiY2RlZmdoaWprbG1u"
	second := testSession(t, secondID, "sessions/second", now.Add(time.Minute))
	_, err := store.Create(testIdentity(), first, now)
	require.NoError(t, err)
	_, err = store.NewConversation(testIdentity(), second, now.Add(time.Minute))
	require.NoError(t, err)

	archives, err := store.ListArchives(testIdentity())
	require.NoError(t, err)
	require.Equal(t, []Session{first}, archives)
	_, err = store.RestoreArchive(testIdentity(), first.ConversationID, now.Add(2*time.Minute))
	require.NoError(t, err)
	loaded, err := store.Load(testIdentity())
	require.NoError(t, err)
	require.Equal(t, first.ConversationID, loaded.Current.ConversationID)
	require.Equal(t, second.ConversationID, loaded.Archives[0].ConversationID)

	_, err = store.RemoveSession(testIdentity(), second.ConversationID, now.Add(3*time.Minute))
	require.NoError(t, err)
	_, err = store.RemoveSession(testIdentity(), first.ConversationID, now.Add(4*time.Minute))
	require.NoError(t, err)
	_, err = store.Load(testIdentity())
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestArchiveHandoffsRequireExactAtomicMappingPrecondition(t *testing.T) {
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	t.Run("new conversation", func(t *testing.T) {
		store := openTestStore(t)
		first := testSession(t, testID, "sessions/first", now)
		second := testSession(t, "MTIzNDU2Nzg5MGFiY2RlZmdoaWprbG1u", "sessions/second", now.Add(time.Minute))
		_, err := store.Create(testIdentity(), first, now)
		require.NoError(t, err)
		expected, err := store.Load(testIdentity())
		require.NoError(t, err)
		revision := Revision{Digest: strings.Repeat("d", 64), Revision: RevisionInitial, SourceUpdatedAt: now.Add(time.Second)}
		_, err = store.ObserveRevision(testIdentity(), revision, now.Add(time.Second))
		require.NoError(t, err)
		outcome, err := store.NewConversationIfUnchanged(testIdentity(), expected, second, now.Add(2*time.Minute))
		require.Equal(t, CommitNotApplied, outcome)
		require.Error(t, err)
		loaded, err := store.Load(testIdentity())
		require.NoError(t, err)
		require.Equal(t, first.ConversationID, loaded.Current.ConversationID)
		require.Equal(t, revision, *loaded.Current.Observed)
		require.Empty(t, loaded.Archives)
	})

	t.Run("accepted promotion", func(t *testing.T) {
		store := openTestStore(t)
		first := testSession(t, testID, "sessions/first", now)
		_, err := store.Create(testIdentity(), first, now)
		require.NoError(t, err)
		revision := Revision{Digest: strings.Repeat("f", 64), Revision: RevisionInitial, SourceUpdatedAt: now.Add(time.Second)}
		_, err = store.PrepareCommit(testIdentity(), revision, testID, now.Add(time.Second))
		require.NoError(t, err)
		_, err = store.MarkPreparedAccepted(testIdentity(), testID, now.Add(2*time.Second))
		require.NoError(t, err)
		expected, err := store.Load(testIdentity())
		require.NoError(t, err)
		_, err = store.Update(testIdentity(), func(mapping *Mapping) error {
			mapping.Current.ModelLabel = "foreign"
			return nil
		})
		require.NoError(t, err)
		outcome, err := store.PromotePreparedIfUnchanged(testIdentity(), expected, testID, now.Add(3*time.Second))
		require.Equal(t, CommitNotApplied, outcome)
		require.Error(t, err)
		loaded, err := store.Load(testIdentity())
		require.NoError(t, err)
		require.Equal(t, "foreign", loaded.Current.ModelLabel)
		require.Equal(t, CommitAccepted, loaded.Current.PreparedCommit.Phase)
	})

	t.Run("prepared reconciliation", func(t *testing.T) {
		store := openTestStore(t)
		first := testSession(t, testID, "sessions/first", now)
		_, err := store.Create(testIdentity(), first, now)
		require.NoError(t, err)
		revision := Revision{Digest: strings.Repeat("9", 64), Revision: RevisionInitial, SourceUpdatedAt: now.Add(time.Second)}
		_, err = store.PrepareCommit(testIdentity(), revision, testID, now.Add(time.Second))
		require.NoError(t, err)
		expected, err := store.Load(testIdentity())
		require.NoError(t, err)
		_, err = store.Update(testIdentity(), func(mapping *Mapping) error {
			mapping.Current.ModelLabel = "foreign"
			return nil
		})
		require.NoError(t, err)
		outcome, err := store.ReconcilePreparedIfUnchanged(testIdentity(), expected, testID, true, now.Add(3*time.Second))
		require.Equal(t, CommitNotApplied, outcome)
		require.Error(t, err)
		loaded, err := store.Load(testIdentity())
		require.NoError(t, err)
		require.Equal(t, "foreign", loaded.Current.ModelLabel)
		require.Equal(t, CommitPrepared, loaded.Current.PreparedCommit.Phase)
	})

	t.Run("restore archive", func(t *testing.T) {
		store := openTestStore(t)
		first := testSession(t, testID, "sessions/first", now)
		second := testSession(t, "MTIzNDU2Nzg5MGFiY2RlZmdoaWprbG1u", "sessions/second", now.Add(time.Minute))
		_, err := store.Create(testIdentity(), first, now)
		require.NoError(t, err)
		_, err = store.NewConversation(testIdentity(), second, now.Add(time.Minute))
		require.NoError(t, err)
		expected, err := store.Load(testIdentity())
		require.NoError(t, err)
		revision := Revision{Digest: strings.Repeat("e", 64), Revision: RevisionInitial, SourceUpdatedAt: now.Add(2 * time.Minute)}
		_, err = store.ObserveRevision(testIdentity(), revision, now.Add(2*time.Minute))
		require.NoError(t, err)
		outcome, err := store.RestoreArchiveIfUnchanged(testIdentity(), expected, first.ConversationID, now.Add(3*time.Minute))
		require.Equal(t, CommitNotApplied, outcome)
		require.Error(t, err)
		loaded, err := store.Load(testIdentity())
		require.NoError(t, err)
		require.Equal(t, second.ConversationID, loaded.Current.ConversationID)
		require.Equal(t, revision, *loaded.Current.Observed)
		require.Equal(t, first.ConversationID, loaded.Archives[0].ConversationID)
	})
}

func TestLoadRejectsCorruptionAndEmbeddedIdentityMismatch(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"unknown field": func(document map[string]any) { document["page_markdown"] = "forbidden" },
		"identity mismatch": func(document map[string]any) {
			document["identity"].(map[string]any)["capability_id"] = "MTIzNDU2Nzg5MGFiY2RlZmdoaWprbG1u"
		},
		"duplicate conversation ID": func(document map[string]any) {
			document["archives"] = []any{document["current"]}
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := openTestStore(t)
			now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
			_, err := store.Create(testIdentity(), testSession(t, testID, "sessions/current", now), now)
			require.NoError(t, err)
			key, err := ConversationKey(testIdentity())
			require.NoError(t, err)
			path := filepath.Join(store.Root(), "conversations", key+".json")
			encoded, err := os.ReadFile(path)
			require.NoError(t, err)
			var document map[string]any
			require.NoError(t, json.Unmarshal(encoded, &document))
			mutate(document)
			encoded, err = json.Marshal(document)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, encoded, 0o600))
			_, err = store.Load(testIdentity())
			require.Error(t, err)
		})
	}
}

func TestStrictDecoderRejectsDuplicateJSONFields(t *testing.T) {
	identity := testIdentity()
	_, err := decodeMapping([]byte(`{"schema_version":2,"schema_version":2}`), identity)
	require.Error(t, err)
}

func TestStrictDecoderRequiresExactNamesAndCanonicalNullability(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	_, err := store.Create(testIdentity(), testSession(t, testID, "sessions/current", now), now)
	require.NoError(t, err)
	key, err := ConversationKey(testIdentity())
	require.NoError(t, err)
	encoded, err := os.ReadFile(filepath.Join(store.Root(), "conversations", key+".json"))
	require.NoError(t, err)

	var canonical map[string]any
	require.NoError(t, json.Unmarshal(encoded, &canonical))
	for name, mutate := range map[string]func(map[string]any){
		"case alias": func(document map[string]any) {
			identity := document["identity"].(map[string]any)
			identity["ORIGIN"] = identity["origin"]
			delete(identity, "origin")
		},
		"null archives": func(document map[string]any) { document["archives"] = nil },
		"missing required nullable field": func(document map[string]any) {
			delete(document["current"].(map[string]any), "observed")
		},
		"null required scalar": func(document map[string]any) {
			document["current"].(map[string]any)["provider_label"] = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			copyEncoded, marshalErr := json.Marshal(canonical)
			require.NoError(t, marshalErr)
			var document map[string]any
			require.NoError(t, json.Unmarshal(copyEncoded, &document))
			mutate(document)
			corrupt, marshalErr := json.Marshal(document)
			require.NoError(t, marshalErr)
			_, decodeErr := decodeMapping(corrupt, testIdentity())
			require.Error(t, decodeErr)
		})
	}
}

func TestNewConversationRejectsDuplicateNativeReferenceWithoutChangingMapping(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	current := testSession(t, testID, "sessions/current", now)
	_, err := store.Create(testIdentity(), current, now)
	require.NoError(t, err)
	duplicate := testSession(t, "MTIzNDU2Nzg5MGFiY2RlZmdoaWprbG1u", "sessions/current", now.Add(time.Minute))
	outcome, err := store.NewConversation(testIdentity(), duplicate, now.Add(time.Minute))
	require.Error(t, err)
	require.Equal(t, CommitNotApplied, outcome)
	loaded, err := store.Load(testIdentity())
	require.NoError(t, err)
	require.Equal(t, testID, loaded.Current.ConversationID)
	require.Empty(t, loaded.Archives)
}

func TestSessionUpdatedAtDoesNotRegress(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	_, err := store.Create(testIdentity(), testSession(t, testID, "sessions/current", now), now)
	require.NoError(t, err)
	revision := Revision{Digest: strings.Repeat("a", 64), Revision: RevisionInitial, SourceUpdatedAt: now}
	_, err = store.ObserveRevision(testIdentity(), revision, now.Add(-time.Hour))
	require.NoError(t, err)
	loaded, err := store.Load(testIdentity())
	require.NoError(t, err)
	require.Equal(t, now, loaded.Current.UpdatedAt)
}

func TestNewConversationRejectsDuplicateIDWithoutChangingMapping(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	current := testSession(t, testID, "sessions/current", now)
	_, err := store.Create(testIdentity(), current, now)
	require.NoError(t, err)
	duplicate := testSession(t, testID, "sessions/duplicate", now.Add(time.Minute))
	outcome, err := store.NewConversation(testIdentity(), duplicate, now.Add(time.Minute))
	require.Error(t, err)
	require.Equal(t, CommitNotApplied, outcome)
	loaded, err := store.Load(testIdentity())
	require.NoError(t, err)
	require.Equal(t, "sessions/current", loaded.Current.NativeSession.Value())
	require.Empty(t, loaded.Archives)
}

func TestConcurrentUpdatesAreSerialized(t *testing.T) {
	store := openTestStore(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	_, err := store.Create(testIdentity(), testSession(t, testID, "sessions/current", now), now)
	require.NoError(t, err)

	var wait sync.WaitGroup
	errors := make(chan error, 24)
	for index := 0; index < 24; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, updateErr := store.Update(testIdentity(), func(mapping *Mapping) error {
				mapping.Current.ModelLabel = time.Unix(int64(index), 0).UTC().Format(time.RFC3339Nano)
				return nil
			})
			errors <- updateErr
		}(index)
	}
	wait.Wait()
	close(errors)
	for updateErr := range errors {
		require.NoError(t, updateErr)
	}
	_, err = store.Load(testIdentity())
	require.NoError(t, err)
}

func TestNativeReferenceValidation(t *testing.T) {
	for _, value := range []string{"", ".", "../session", "sessions/../session", "/absolute", "session\x00bad", strings.Repeat("x", 1025)} {
		_, err := NativeSessionRef(value)
		require.Error(t, err, value)
	}
	ref, err := NativeSessionRef("sessions/pi-session.jsonl")
	require.NoError(t, err)
	require.Equal(t, "sessions/pi-session.jsonl", ref.Value())
}

func TestWorkspaceAndProviderDirectories(t *testing.T) {
	store := openTestStore(t)
	workspace, err := store.EnsureWorkspace(testID)
	require.NoError(t, err)
	providerRoot, err := store.EnsureProviderDirectory(provider.NamePi)
	require.NoError(t, err)
	codexRoot, err := store.EnsureProviderDirectory(provider.NameCodex)
	require.NoError(t, err)
	for _, path := range []string{workspace, providerRoot, codexRoot} {
		info, statErr := os.Stat(path)
		require.NoError(t, statErr)
		require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	}
	require.NoError(t, store.RemoveWorkspace(testID))
	_, err = os.Stat(workspace)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func sortedKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func independentSHA256Hex(value []byte) string {
	// Kept separate from ConversationKey's framing to make the vector meaningful.
	hash := sha256.Sum256(value)
	return hex.EncodeToString(hash[:])
}
