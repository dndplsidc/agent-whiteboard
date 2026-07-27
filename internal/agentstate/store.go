package agentstate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/provider"
)

type CommitOutcome string

const (
	CommitNotApplied CommitOutcome = "not_applied"
	CommitApplied    CommitOutcome = "applied"
	CommitUncertain  CommitOutcome = "uncertain"
)

var (
	ErrLocked      = errors.New("agent state is locked by another broker")
	ErrUnsupported = errors.New("agent state is supported only on macOS and Linux")
	ErrClosed      = errors.New("agent state store is closed")
)

type Store struct {
	rootPath      string
	layout        *stateLayout
	conversations *secureDirectory
	workspaces    *secureDirectory
	providers     *secureDirectory
	ops           fileOps
	locks         keyedLocks
	lifecycle     sync.RWMutex
	closed        bool
}

type keyedLocks struct {
	mu      sync.Mutex
	entries map[string]*keyedLock
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

// Open opens the fixed .agent-whiteboard/state tree beneath home. Passing an
// empty home uses the current user's home directory; home is injectable so
// callers and tests can isolate the fixed layout without adding a state-root
// configuration surface.
func Open(home string) (*Store, error) {
	if home == "" {
		resolved, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = resolved
	}
	layout, err := openStateLayout(home)
	if err != nil {
		return nil, err
	}
	store := &Store{
		rootPath: layout.rootPath, layout: layout, conversations: layout.conversations,
		workspaces: layout.workspaces, providers: layout.providers,
		locks: keyedLocks{entries: make(map[string]*keyedLock)},
	}
	store.ops = defaultFileOps(store.conversations)
	if err := cleanTemporaryMappings(store.conversations); err != nil {
		_ = layout.close()
		return nil, fmt.Errorf("clean temporary mappings: %w", err)
	}
	return store, nil
}

func (store *Store) Root() string { return store.rootPath }

func (store *Store) Close() error {
	store.lifecycle.Lock()
	defer store.lifecycle.Unlock()
	if store.closed {
		return nil
	}
	store.closed = true
	return store.layout.close()
}

func (store *Store) Load(identity Identity) (Mapping, error) {
	key, err := ConversationKey(identity)
	if err != nil {
		return Mapping{}, err
	}
	release, err := store.begin(key)
	if err != nil {
		return Mapping{}, err
	}
	defer release()
	mapping, _, err := store.loadLocked(identity, key)
	return mapping, err
}

func (store *Store) Create(identity Identity, current Session, at time.Time) (CommitOutcome, error) {
	key, err := ConversationKey(identity)
	if err != nil {
		return CommitNotApplied, err
	}
	release, err := store.begin(key)
	if err != nil {
		return CommitNotApplied, err
	}
	defer release()
	if _, err := store.conversations.targetIdentity(mappingName(key)); err == nil {
		return CommitNotApplied, os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return CommitNotApplied, fmt.Errorf("inspect conversation mapping: %w", err)
	}
	mapping := Mapping{SchemaVersion: SchemaVersion, Identity: identity, Current: &current, Archives: []Session{}, CreatedAt: at, UpdatedAt: at}
	return store.commitLocked(key, mapping, nil)
}

// Update serializes one complete mapping replacement. The callback must only
// modify approved bookkeeping fields; identity and creation time are retained.
func (store *Store) Update(identity Identity, update func(*Mapping) error) (CommitOutcome, error) {
	if update == nil {
		return CommitNotApplied, errors.New("mapping update is required")
	}
	return store.updateAt(identity, time.Now().UTC(), update)
}

func (store *Store) updateAt(identity Identity, at time.Time, update func(*Mapping) error) (CommitOutcome, error) {
	key, err := ConversationKey(identity)
	if err != nil {
		return CommitNotApplied, err
	}
	release, err := store.begin(key)
	if err != nil {
		return CommitNotApplied, err
	}
	defer release()
	mapping, original, err := store.loadLocked(identity, key)
	if err != nil {
		return CommitNotApplied, err
	}
	originalIdentity := mapping.Identity
	originalCreatedAt := mapping.CreatedAt
	if err := update(&mapping); err != nil {
		return CommitNotApplied, err
	}
	mapping.SchemaVersion = SchemaVersion
	mapping.Identity = originalIdentity
	mapping.CreatedAt = originalCreatedAt
	if at.Before(mapping.UpdatedAt) {
		at = mapping.UpdatedAt
	}
	mapping.UpdatedAt = at
	return store.commitLocked(key, mapping, &original)
}

func (store *Store) NewConversation(identity Identity, current Session, at time.Time) (CommitOutcome, error) {
	return store.updateAt(identity, at, func(mapping *Mapping) error {
		if mapping.Current != nil {
			mapping.Archives = append(mapping.Archives, *mapping.Current)
		}
		mapping.Current = &current
		return nil
	})
}

func (store *Store) RestoreArchive(identity Identity, conversationID string, at time.Time) (CommitOutcome, error) {
	if common.ValidateID(conversationID) != nil {
		return CommitNotApplied, errors.New("invalid archive ID")
	}
	return store.updateAt(identity, at, func(mapping *Mapping) error {
		index := archiveIndex(mapping.Archives, conversationID)
		if index < 0 {
			return os.ErrNotExist
		}
		restored := mapping.Archives[index]
		mapping.Archives = append(mapping.Archives[:index], mapping.Archives[index+1:]...)
		if mapping.Current != nil {
			mapping.Archives = append(mapping.Archives, *mapping.Current)
		}
		mapping.Current = &restored
		return nil
	})
}

func (store *Store) ListArchives(identity Identity) ([]Session, error) {
	mapping, err := store.Load(identity)
	if err != nil {
		return nil, err
	}
	archives := append([]Session(nil), mapping.Archives...)
	sort.SliceStable(archives, func(left, right int) bool {
		return archives[left].UpdatedAt.After(archives[right].UpdatedAt)
	})
	return archives, nil
}

// RemoveSession performs only the final atomic mapping transition. Callers
// invoke it after live-process, provider-native, and workspace removal succeed.
func (store *Store) RemoveSession(identity Identity, conversationID string, at time.Time) (CommitOutcome, error) {
	if common.ValidateID(conversationID) != nil {
		return CommitNotApplied, errors.New("invalid conversation ID")
	}
	key, err := ConversationKey(identity)
	if err != nil {
		return CommitNotApplied, err
	}
	release, err := store.begin(key)
	if err != nil {
		return CommitNotApplied, err
	}
	defer release()
	mapping, original, err := store.loadLocked(identity, key)
	if err != nil {
		return CommitNotApplied, err
	}
	removed := false
	if mapping.Current != nil && mapping.Current.ConversationID == conversationID {
		mapping.Current = nil
		removed = true
	} else if index := archiveIndex(mapping.Archives, conversationID); index >= 0 {
		mapping.Archives = append(mapping.Archives[:index], mapping.Archives[index+1:]...)
		removed = true
	}
	if !removed {
		return CommitNotApplied, os.ErrNotExist
	}
	if mapping.Current == nil && len(mapping.Archives) == 0 {
		return store.removeLocked(key, original)
	}
	mapping.UpdatedAt = at
	return store.commitLocked(key, mapping, &original)
}

func (store *Store) Remove(identity Identity) (CommitOutcome, error) {
	key, err := ConversationKey(identity)
	if err != nil {
		return CommitNotApplied, err
	}
	release, err := store.begin(key)
	if err != nil {
		return CommitNotApplied, err
	}
	defer release()
	_, original, err := store.loadLocked(identity, key)
	if err != nil {
		return CommitNotApplied, err
	}
	return store.removeLocked(key, original)
}

func (store *Store) ObserveRevision(identity Identity, revision Revision, at time.Time) (CommitOutcome, error) {
	if err := revision.validate(); err != nil {
		return CommitNotApplied, err
	}
	return store.updateAt(identity, at, func(mapping *Mapping) error {
		if mapping.Current == nil {
			return os.ErrNotExist
		}
		observed := revision
		mapping.Current.Observed = &observed
		mapping.Current.UpdatedAt = at
		return nil
	})
}

func (store *Store) PrepareCommit(identity Identity, revision Revision, turnID string, at time.Time) (CommitOutcome, error) {
	prepared := PreparedCommit{Revision: revision, TurnID: turnID, Phase: CommitPrepared}
	if err := prepared.validate(); err != nil {
		return CommitNotApplied, err
	}
	return store.updateAt(identity, at, func(mapping *Mapping) error {
		if mapping.Current == nil {
			return os.ErrNotExist
		}
		observed := revision
		mapping.Current.Observed = &observed
		mapping.Current.PreparedCommit = &prepared
		mapping.Current.UpdatedAt = at
		return nil
	})
}

func (store *Store) PromotePrepared(identity Identity, turnID string, at time.Time) (CommitOutcome, error) {
	return store.reconcilePrepared(identity, turnID, true, at)
}

func (store *Store) MarkPreparedAccepted(identity Identity, turnID string, at time.Time) (CommitOutcome, error) {
	return store.updateAt(identity, at, func(mapping *Mapping) error {
		prepared, err := expectedPrepared(mapping, turnID)
		if err != nil {
			return err
		}
		prepared.Phase = CommitAccepted
		mapping.Current.UpdatedAt = at
		return nil
	})
}

func (store *Store) ReconcilePrepared(identity Identity, turnID string, accepted bool, at time.Time) (CommitOutcome, error) {
	return store.reconcilePrepared(identity, turnID, accepted, at)
}

func (store *Store) reconcilePrepared(identity Identity, turnID string, accepted bool, at time.Time) (CommitOutcome, error) {
	if common.ValidateID(turnID) != nil {
		return CommitNotApplied, errors.New("invalid turn ID")
	}
	return store.updateAt(identity, at, func(mapping *Mapping) error {
		prepared, err := expectedPrepared(mapping, turnID)
		if err != nil {
			return err
		}
		if accepted {
			committed := prepared.Revision
			mapping.Current.Committed = &committed
			mapping.Current.Observed = nil
		} else {
			observed := prepared.Revision
			mapping.Current.Observed = &observed
		}
		mapping.Current.PreparedCommit = nil
		mapping.Current.UpdatedAt = at
		return nil
	})
}

func expectedPrepared(mapping *Mapping, turnID string) (*PreparedCommit, error) {
	if mapping.Current == nil || mapping.Current.PreparedCommit == nil || mapping.Current.PreparedCommit.TurnID != turnID {
		return nil, os.ErrNotExist
	}
	return mapping.Current.PreparedCommit, nil
}

func (store *Store) EnsureWorkspace(conversationID string) (string, error) {
	if common.ValidateID(conversationID) != nil {
		return "", errors.New("invalid conversation ID")
	}
	release, err := store.begin("workspace/" + conversationID)
	if err != nil {
		return "", err
	}
	defer release()
	directory, _, err := store.workspaces.ensureDirectory(conversationID, true)
	if err != nil {
		return "", fmt.Errorf("ensure workspace: %w", err)
	}
	if err := directory.close(); err != nil {
		return "", err
	}
	return filepath.Join(store.rootPath, "workspaces", conversationID), nil
}

func (store *Store) RemoveWorkspace(conversationID string) error {
	if common.ValidateID(conversationID) != nil {
		return errors.New("invalid conversation ID")
	}
	release, err := store.begin("workspace/" + conversationID)
	if err != nil {
		return err
	}
	defer release()
	if err := store.workspaces.removeDirectory(conversationID); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (store *Store) EnsureProviderDirectory(name provider.Name) (string, error) {
	if name != provider.NamePi {
		return "", errors.New("invalid provider")
	}
	release, err := store.begin("provider/" + string(name))
	if err != nil {
		return "", err
	}
	defer release()
	directory, _, err := store.providers.ensureDirectory(string(name), true)
	if err != nil {
		return "", fmt.Errorf("ensure provider directory: %w", err)
	}
	if err := directory.close(); err != nil {
		return "", err
	}
	return filepath.Join(store.rootPath, "providers", string(name)), nil
}

func (store *Store) loadLocked(identity Identity, key string) (Mapping, fileIdentity, error) {
	encoded, fileID, err := store.conversations.readVerified(mappingName(key), maxMappingBytes)
	if err != nil {
		return Mapping{}, fileIdentity{}, err
	}
	mapping, err := decodeMapping(encoded, identity)
	return mapping, fileID, err
}

func (store *Store) commitLocked(key string, mapping Mapping, original *fileIdentity) (outcome CommitOutcome, resultErr error) {
	encoded, err := encodeMapping(mapping)
	if err != nil {
		return CommitNotApplied, err
	}
	temporary, temporaryName, err := store.conversations.createTemporary()
	if err != nil {
		return CommitNotApplied, err
	}
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = store.conversations.remove(temporaryName)
		}
	}()
	for remaining := encoded; len(remaining) > 0; {
		written, err := temporary.Write(remaining)
		if err != nil {
			return CommitNotApplied, err
		}
		if written == 0 {
			return CommitNotApplied, io.ErrShortWrite
		}
		remaining = remaining[written:]
	}
	if err := temporary.Sync(); err != nil {
		return CommitNotApplied, err
	}
	if err := temporary.Close(); err != nil {
		return CommitNotApplied, err
	}
	name := mappingName(key)
	if err := store.verifyTarget(name, original); err != nil {
		return CommitNotApplied, err
	}
	if err := store.ops.rename(temporaryName, name); err != nil {
		outcome := inspectExpected(store.conversations, name, encoded)
		if outcome == CommitApplied {
			committed = true
		}
		return outcome, fmt.Errorf("publish conversation mapping: %w", err)
	}
	committed = true
	if err := store.ops.syncDir(); err != nil {
		return CommitUncertain, fmt.Errorf("sync conversation directory: %w", err)
	}
	return CommitApplied, nil
}

func (store *Store) removeLocked(key string, original fileIdentity) (CommitOutcome, error) {
	name := mappingName(key)
	if err := store.verifyTarget(name, &original); err != nil {
		return CommitNotApplied, err
	}
	if err := store.ops.remove(name); err != nil {
		if _, inspectErr := store.conversations.targetIdentity(name); errors.Is(inspectErr, os.ErrNotExist) {
			return CommitApplied, fmt.Errorf("remove conversation mapping: %w", err)
		} else if inspectErr != nil {
			return CommitUncertain, fmt.Errorf("remove conversation mapping: %w", err)
		}
		return CommitNotApplied, fmt.Errorf("remove conversation mapping: %w", err)
	}
	if err := store.ops.syncDir(); err != nil {
		return CommitUncertain, fmt.Errorf("sync conversation directory: %w", err)
	}
	return CommitApplied, nil
}

func (store *Store) verifyTarget(name string, original *fileIdentity) error {
	current, err := store.conversations.targetIdentity(name)
	if original == nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		return os.ErrExist
	}
	if err != nil || current != *original {
		return errors.New("conversation mapping changed during update")
	}
	return nil
}

func (store *Store) begin(key string) (func(), error) {
	store.lifecycle.RLock()
	if store.closed {
		store.lifecycle.RUnlock()
		return nil, ErrClosed
	}
	store.locks.mu.Lock()
	entry := store.locks.entries[key]
	if entry == nil {
		entry = &keyedLock{}
		store.locks.entries[key] = entry
	}
	entry.refs++
	store.locks.mu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		store.locks.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(store.locks.entries, key)
		}
		store.locks.mu.Unlock()
		store.lifecycle.RUnlock()
	}, nil
}

func archiveIndex(archives []Session, conversationID string) int {
	for index := range archives {
		if archives[index].ConversationID == conversationID {
			return index
		}
	}
	return -1
}

func mappingName(key string) string { return key + ".json" }

var _ io.Closer = (*Store)(nil)
