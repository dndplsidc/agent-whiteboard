package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	markdownID = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	htmlID     = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
)

type fixedClock struct{ at time.Time }

func (clock fixedClock) Now() time.Time { return clock.at }

func TestStoreSaveListAndIdentity(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	store := newTestStore(t, now)
	records := []Record{
		validRecord("http://example.test", KindMarkdown, markdownID, "Same title", "first summary", "one.md", now.Add(-time.Minute), StateCreated),
		validRecord("http://other.test:8080", KindHTML, htmlID, "Same title", "second recharge summary", "two.html", now, StateCreationUncertain),
	}
	records[0].ExpiresAt = int64Pointer(now.Add(-time.Second).Unix())
	records[0].Permanent = false
	for _, record := range records {
		require.NoError(t, store.SaveCreated(context.Background(), record))
	}

	page, err := store.List(context.Background(), Query{Limit: 20})
	require.NoError(t, err)
	require.Equal(t, 2, page.Total)
	require.Equal(t, records[1].ID, page.Records[0].ID)
	require.True(t, page.Records[1].Expired)
	require.Equal(t, StateCreationUncertain, page.Records[0].State)

	page, err = store.List(context.Background(), Query{Kind: KindHTML, Terms: "same recharge", Limit: 1})
	require.NoError(t, err)
	require.Equal(t, 1, page.Total)
	require.Len(t, page.Records, 1)
	require.Equal(t, htmlID, page.Records[0].ID)

	page, err = store.List(context.Background(), Query{Terms: "FIRST title", Limit: 20})
	require.NoError(t, err)
	require.Equal(t, 1, page.Total)
	require.Equal(t, markdownID, page.Records[0].ID)

	serverKey := ServerKey("http://example.test")
	firstPath := filepath.Join(store.rootPath, "entries", serverKey, string(KindMarkdown), markdownID+".json")
	secondPath := filepath.Join(store.rootPath, "entries", ServerKey("http://other.test:8080"), string(KindHTML), htmlID+".json")
	require.FileExists(t, firstPath)
	require.FileExists(t, secondPath)
	require.NotEqual(t, firstPath, secondPath)
	assertPrivatePath(t, store.rootPath, 0o700)
	assertPrivatePath(t, filepath.Join(store.rootPath, "entries"), 0o700)
	assertPrivatePath(t, filepath.Dir(filepath.Dir(firstPath)), 0o700)
	assertPrivatePath(t, filepath.Dir(firstPath), 0o700)
	assertPrivatePath(t, firstPath, 0o600)
	assertPrivatePath(t, filepath.Join(filepath.Dir(firstPath), markdownID+".lock"), 0o600)
}

func TestStoreKeepsModesServersAndDeterministicTiesDistinct(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	store := newTestStore(t, now)
	records := []Record{
		validRecord("https://z.example", KindMarkdown, markdownID, "Café recharge", "Résumé target", "one.md", now, StateCreated),
		validRecord("https://a.example", KindHTML, markdownID, "Another", "Target", "one.html", now, StateCreated),
		validRecord("https://a.example", KindMarkdown, htmlID, "Third", "Target", "two.md", now, StateCreated),
	}
	for _, record := range records {
		require.NoError(t, store.SaveCreated(context.Background(), record))
	}

	page, err := store.List(context.Background(), Query{Terms: "CAFÉ résumé", Limit: 20})
	require.NoError(t, err)
	require.Equal(t, []string{markdownID}, resultIDs(page.Records))

	page, err = store.List(context.Background(), Query{Limit: 20})
	require.NoError(t, err)
	require.Equal(t, []string{markdownID, htmlID, markdownID}, resultIDs(page.Records))
	require.Equal(t, []Kind{KindHTML, KindMarkdown, KindMarkdown}, []Kind{page.Records[0].Kind, page.Records[1].Kind, page.Records[2].Kind})
	require.NotEqual(t,
		filepath.Join(store.rootPath, "entries", ServerKey("https://a.example"), string(KindHTML), markdownID+".json"),
		filepath.Join(store.rootPath, "entries", ServerKey("https://z.example"), string(KindMarkdown), markdownID+".json"),
	)
}

func TestStoreConcurrentDifferentIdentitiesDoNotLoseRecords(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	store := newTestStore(t, now)
	const count = 12
	var group sync.WaitGroup
	errors := make(chan error, count)
	for index := 0; index < count; index++ {
		index := index
		group.Add(1)
		go func() {
			defer group.Done()
			id := fmt.Sprintf("%032d", index+1)
			errors <- store.SaveCreated(context.Background(), validRecord("https://example.test", KindMarkdown, id, "Concurrent", "Independent identity", fmt.Sprintf("%d.md", index), now, StateCreated))
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	page, err := store.List(context.Background(), Query{Limit: count})
	require.NoError(t, err)
	require.Equal(t, count, page.Total)
}

func TestStoreUpdateDeleteAndPagination(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	store := newTestStore(t, now)
	first := validRecord("https://example.test", KindMarkdown, markdownID, "First", "Summary", "one.md", now.Add(-time.Minute), StateCreated)
	second := validRecord("https://example.test", KindHTML, htmlID, "Second", "Summary", "two.html", now, StateCreated)
	require.NoError(t, store.SaveCreated(context.Background(), first))
	require.NoError(t, store.SaveCreated(context.Background(), second))

	entry, found, err := store.OpenExisting(context.Background(), first.Identity())
	require.NoError(t, err)
	require.True(t, found)
	updated := entry.Record()
	updated.Title = "Updated"
	updated.SourceFilename = "updated.md"
	updated.UpdatedAt = now.Add(time.Hour).Unix()
	require.NoError(t, entry.Save(context.Background(), updated))
	require.NoError(t, entry.Close())

	entry, found, err = store.OpenExisting(context.Background(), first.Identity())
	require.NoError(t, err)
	require.True(t, found)
	deleted := entry.Record()
	deleted.State = StateDeleted
	deletedAt := now.Add(2 * time.Hour).Unix()
	deleted.DeletedAt = &deletedAt
	deleted.UpdatedAt = deletedAt
	require.NoError(t, entry.Save(context.Background(), deleted))
	require.NoError(t, entry.Close())

	page, err := store.List(context.Background(), Query{Limit: 1, Offset: 1})
	require.NoError(t, err)
	require.Equal(t, 2, page.Total)
	require.Len(t, page.Records, 1)
	require.Equal(t, markdownID, page.Records[0].ID)
	require.Equal(t, StateDeleted, page.Records[0].State)
	require.Equal(t, first.CreatedAt, page.Records[0].CreatedAt)
}

func TestStoreLargeMetadataLifecycle(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	store := newTestStore(t, now)
	record := validRecord("https://example.test", KindMarkdown, markdownID, strings.Repeat("<", 100_000), strings.Repeat(">", 100_000), "large.md", now, StateCreated)
	require.NoError(t, store.SaveCreated(context.Background(), record))
	for _, state := range []State{StateCreated, StateDeleted} {
		page, err := store.List(context.Background(), Query{Limit: 20})
		require.NoError(t, err)
		require.Len(t, page.Records, 1)
		require.Equal(t, record, page.Records[0].Record)
		entry, found, err := store.OpenExisting(context.Background(), record.Identity())
		require.NoError(t, err)
		require.True(t, found)
		if state == StateDeleted {
			require.NoError(t, entry.MarkDeleted(context.Background()))
		} else {
			title := record.Title + "updated"
			require.NoError(t, entry.Update(context.Background(), Update{URL: record.URL, SourceFilename: "updated.md", Title: &title, Permanent: true}))
		}
		record = entry.Record()
		require.NoError(t, entry.Close())
	}
	page, err := store.List(context.Background(), Query{Limit: 20})
	require.NoError(t, err)
	require.Equal(t, record, page.Records[0].Record)
	require.Equal(t, StateDeleted, page.Records[0].State)
}

func TestStorePaginationCannotOverflow(t *testing.T) {
	store := newTestStore(t, time.Unix(2_000_000_000, 0).UTC())
	for _, id := range []string{markdownID, htmlID} {
		require.NoError(t, store.SaveCreated(context.Background(), validRecord("https://example.test", KindMarkdown, id, "Title", "Summary", "one.md", store.clock.Now(), StateCreated)))
	}
	for _, offset := range []int{1, 2, 3, math.MaxInt} {
		t.Run(fmt.Sprint(offset), func(t *testing.T) {
			var page Page
			var err error
			require.NotPanics(t, func() { page, err = store.List(context.Background(), Query{Limit: math.MaxInt, Offset: offset}) })
			require.NoError(t, err)
			require.Equal(t, 2, page.Total)
			require.Len(t, page.Records, max(0, 2-offset))
		})
	}
}

func TestStoreListMissingMalformedAndUnsupported(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	root := filepath.Join(t.TempDir(), "catalog")
	store, err := New(Config{Root: root, Clock: fixedClock{at: now}})
	require.NoError(t, err)
	page, err := store.List(context.Background(), Query{Limit: 20})
	require.NoError(t, err)
	require.Empty(t, page.Records)
	require.NoDirExists(t, root)

	require.NoError(t, store.SaveCreated(context.Background(), validRecord("https://example.test", KindMarkdown, markdownID, "Title", "Summary", "one.md", now, StateCreated)))
	path := filepath.Join(root, "entries", ServerKey("https://example.test"), string(KindMarkdown), markdownID+".json")
	require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))
	_, err = store.List(context.Background(), Query{Limit: 20})
	require.ErrorContains(t, err, "read catalog record")

	record := validRecord("https://example.test", KindMarkdown, markdownID, "Title", "Summary", "one.md", now, StateCreated)
	record.SchemaVersion = 2
	encoded, marshalErr := json.Marshal(record)
	require.NoError(t, marshalErr)
	require.NoError(t, os.WriteFile(path, encoded, 0o600))
	_, err = store.List(context.Background(), Query{Limit: 20})
	require.ErrorContains(t, err, "unsupported catalog schema version")
}

func TestStoreRejectsUnsafePathsAndRecords(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	require.NoError(t, os.Mkdir(real, 0o700))
	root := filepath.Join(parent, "catalog")
	require.NoError(t, os.Symlink(real, root))
	store, err := New(Config{Root: root, Clock: fixedClock{at: now}})
	require.NoError(t, err)
	require.Error(t, store.Prepare(context.Background(), "https://example.test", KindMarkdown))

	store = newTestStore(t, now)
	bad := validRecord("https://example.test", KindMarkdown, markdownID, " ", "Summary", "one.md", now, StateCreated)
	require.Error(t, store.SaveCreated(context.Background(), bad))
	bad = validRecord("https://example.test", KindMarkdown, "../escape", "Title", "Summary", "one.md", now, StateCreated)
	require.Error(t, store.SaveCreated(context.Background(), bad))
	bad = validRecord("https://example.test", KindMarkdown, markdownID, "Title", "Summary", "/private/source.md", now, StateCreated)
	require.Error(t, store.SaveCreated(context.Background(), bad))

	valid := validRecord("https://example.test", KindMarkdown, markdownID, "Title", "Summary", "source.md", now, StateCreated)
	require.NoError(t, store.SaveCreated(context.Background(), valid))
	kindRoot := filepath.Join(store.rootPath, "entries", ServerKey(valid.Server), string(valid.Kind))
	require.NoError(t, os.Remove(filepath.Join(kindRoot, markdownID+".json")))
	require.NoError(t, os.Symlink(filepath.Join(parent, "outside-record"), filepath.Join(kindRoot, markdownID+".json")))
	_, _, err = store.OpenExisting(context.Background(), valid.Identity())
	require.ErrorContains(t, err, "record")
	require.NoError(t, os.Remove(filepath.Join(kindRoot, markdownID+".json")))
	encoded, marshalErr := json.Marshal(valid)
	require.NoError(t, marshalErr)
	require.NoError(t, os.WriteFile(filepath.Join(kindRoot, markdownID+".json"), encoded, 0o600))
	require.NoError(t, os.Remove(filepath.Join(kindRoot, markdownID+".lock")))
	require.NoError(t, os.Symlink(filepath.Join(parent, "outside-lock"), filepath.Join(kindRoot, markdownID+".lock")))
	_, _, err = store.OpenExisting(context.Background(), valid.Identity())
	require.ErrorContains(t, err, "lock")
}

func TestPrepareChecksTargetKindWithoutChangingExistingPermissions(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	store := newTestStore(t, now)
	require.NoError(t, store.Prepare(context.Background(), "https://example.test", KindMarkdown))
	kindRoot := filepath.Join(store.rootPath, "entries", ServerKey("https://example.test"), string(KindMarkdown))
	require.NoError(t, os.Chmod(kindRoot, 0o500))
	t.Cleanup(func() { _ = os.Chmod(kindRoot, 0o700) })

	require.Error(t, store.Prepare(context.Background(), "https://example.test", KindMarkdown))
	assertPrivatePath(t, kindRoot, 0o500)
}

func TestOpenExistingChecksWritabilityWithExistingLock(t *testing.T) {
	store := newTestStore(t, time.Unix(2_000_000_000, 0))
	record := validRecord("https://example.test", KindMarkdown, markdownID, "Title", "Summary", "one.md", store.clock.Now(), StateCreated)
	require.NoError(t, store.SaveCreated(context.Background(), record))
	kindRoot := filepath.Join(store.rootPath, "entries", ServerKey(record.Server), string(record.Kind))
	assertPrivatePath(t, filepath.Join(kindRoot, record.ID+".lock"), 0o600)
	require.NoError(t, os.Chmod(kindRoot, 0o500))
	t.Cleanup(func() { _ = os.Chmod(kindRoot, 0o700) })
	entry, _, err := store.OpenExisting(context.Background(), record.Identity())
	if entry != nil {
		defer entry.Close()
	}
	require.Error(t, err)
	assertPrivatePath(t, kindRoot, 0o500)
}

func TestOpenExistingLockHonorsCancellation(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	store := newTestStore(t, now)
	record := validRecord("https://example.test", KindMarkdown, markdownID, "Title", "Summary", "one.md", now, StateCreated)
	require.NoError(t, store.SaveCreated(context.Background(), record))
	first, found, err := store.OpenExisting(context.Background(), record.Identity())
	require.NoError(t, err)
	require.True(t, found)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = store.OpenExisting(ctx, record.Identity())
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, first.Close())

	second, found, err := store.OpenExisting(context.Background(), record.Identity())
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, second.Close())
}

func TestStoreRejectsInvalidQuery(t *testing.T) {
	store := newTestStore(t, time.Unix(2_000_000_000, 0).UTC())
	for _, query := range []Query{{Kind: "image", Limit: 20}, {Limit: 0}, {Limit: -1}, {Limit: 20, Offset: -1}} {
		_, err := store.List(context.Background(), query)
		require.Error(t, err)
	}
}

func newTestStore(t *testing.T, now time.Time) *Store {
	t.Helper()
	store, err := New(Config{Root: filepath.Join(t.TempDir(), "catalog"), Clock: fixedClock{at: now}})
	require.NoError(t, err)
	return store
}

func validRecord(server string, kind Kind, id, title, summary, source string, created time.Time, state State) Record {
	return Record{
		SchemaVersion:  1,
		Server:         server,
		Kind:           kind,
		ID:             id,
		URL:            server + "/whiteboards/" + string(kind) + "/" + id,
		Title:          title,
		Summary:        summary,
		SourceFilename: source,
		CreatedAt:      created.Unix(),
		UpdatedAt:      created.Unix(),
		Permanent:      true,
		State:          state,
	}
}

func assertPrivatePath(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	require.NoError(t, err)
	require.Equal(t, want, info.Mode().Perm())
}

func int64Pointer(value int64) *int64 { return &value }

func resultIDs(records []ResultRecord) []string {
	ids := make([]string, len(records))
	for index, record := range records {
		ids[index] = record.ID
	}
	return ids
}
