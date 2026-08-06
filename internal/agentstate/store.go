package agentstate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
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

	openHomes = struct {
		sync.Mutex
		active map[string]struct{}
	}{active: make(map[string]struct{})}
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
	homeGuardKey  string
	invalidLayout atomic.Bool
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
// configuration surface. The lifetime lock coordinates cooperating brokers.
// A hostile same-UID process that ignores locks and replaces pathnames remains
// outside the guarantee; detected replacement makes this Store fail closed.
func Open(home string) (*Store, error) {
	if home == "" {
		resolved, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = resolved
	}
	absolute, err := filepath.Abs(home)
	if err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve canonical home: %w", err)
	}
	openHomes.Lock()
	if _, exists := openHomes.active[canonical]; exists {
		openHomes.Unlock()
		return nil, ErrLocked
	}
	openHomes.active[canonical] = struct{}{}
	openHomes.Unlock()
	releaseHome := true
	defer func() {
		if releaseHome {
			releaseOpenHome(canonical)
		}
	}()
	layout, err := openStateLayout(canonical)
	if err != nil {
		return nil, err
	}
	store := &Store{
		rootPath: layout.rootPath, layout: layout, conversations: layout.conversations,
		workspaces: layout.workspaces, providers: layout.providers,
		locks: keyedLocks{entries: make(map[string]*keyedLock)}, homeGuardKey: canonical,
	}
	store.ops = defaultFileOps(store.conversations, store.workspaces)
	for _, directory := range []*secureDirectory{store.conversations, store.workspaces, store.providers} {
		directory.setMutationGuard(store.verifyLayout)
	}
	if err := cleanTemporaryMappings(store.conversations); err != nil {
		_ = layout.close()
		return nil, fmt.Errorf("clean temporary mappings: %w", err)
	}
	if err := layout.verify(); err != nil {
		_ = layout.close()
		return nil, fmt.Errorf("verify canonical state layout: %w", err)
	}
	releaseHome = false
	return store, nil
}

func releaseOpenHome(key string) {
	openHomes.Lock()
	delete(openHomes.active, key)
	openHomes.Unlock()
}

func (store *Store) Root() string { return store.rootPath }

func (store *Store) Close() error {
	store.lifecycle.Lock()
	defer store.lifecycle.Unlock()
	if store.closed {
		return nil
	}
	store.closed = true
	err := store.layout.close()
	releaseOpenHome(store.homeGuardKey)
	store.homeGuardKey = ""
	return err
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
	if err == nil {
		err = store.verifyLayout()
	}
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
	return store.updateAtExpected(identity, nil, at, update)
}

func (store *Store) updateAtExpected(identity Identity, expected *Mapping, at time.Time, update func(*Mapping) error) (CommitOutcome, error) {
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
	if expected != nil && !reflect.DeepEqual(mapping, *expected) {
		return CommitNotApplied, errors.New("conversation mapping changed")
	}
	originalIdentity := mapping.Identity
	originalCreatedAt := mapping.CreatedAt
	before := snapshotSessions(mapping)
	if err := update(&mapping); err != nil {
		return CommitNotApplied, err
	}
	clampSessionTimes(&mapping, before)
	if err := validateSessionTransitions(mapping, before); err != nil {
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

// NewConversationIfUnchanged atomically archives the expected current mapping
// and installs current. A changed mapping is never mutated.
func (store *Store) NewConversationIfUnchanged(identity Identity, expected Mapping, current Session, at time.Time) (CommitOutcome, error) {
	if expected.Validate(identity) != nil {
		return CommitNotApplied, errors.New("invalid expected conversation mapping")
	}
	return store.updateAtExpected(identity, &expected, at, func(mapping *Mapping) error {
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

// RestoreArchiveIfUnchanged atomically restores conversationID only when the
// complete mapping still equals expected.
func (store *Store) RestoreArchiveIfUnchanged(identity Identity, expected Mapping, conversationID string, at time.Time) (CommitOutcome, error) {
	if common.ValidateID(conversationID) != nil || expected.Validate(identity) != nil {
		return CommitNotApplied, errors.New("invalid archive restore precondition")
	}
	return store.updateAtExpected(identity, &expected, at, func(mapping *Mapping) error {
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
	mapping.UpdatedAt = laterTime(mapping.UpdatedAt, at)
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
		if prepared := mapping.Current.PreparedCommit; prepared != nil && prepared.Revision != revision {
			return errors.New("cannot replace an unresolved prepared revision")
		}
		observed := revision
		mapping.Current.Observed = &observed
		mapping.Current.UpdatedAt = laterTime(mapping.Current.UpdatedAt, at)
		return nil
	})
}

// AcknowledgeCommittedRevision clears a superseded pending observation when a
// newer source revision returns to the already-committed digest. No provider
// context commit is required because the committed content is unchanged.
func (store *Store) AcknowledgeCommittedRevision(identity Identity, revision Revision, at time.Time) (CommitOutcome, error) {
	if err := revision.validate(); err != nil {
		return CommitNotApplied, err
	}
	return store.updateAt(identity, at, func(mapping *Mapping) error {
		if mapping.Current == nil || mapping.Current.Committed == nil || mapping.Current.Observed == nil {
			return os.ErrNotExist
		}
		if mapping.Current.PreparedCommit != nil {
			return errors.New("cannot acknowledge an unresolved prepared revision")
		}
		if revision.Revision != RevisionReplacement || mapping.Current.Committed.Digest != revision.Digest || mapping.Current.Observed.Digest == revision.Digest || !revision.SourceUpdatedAt.After(mapping.Current.Observed.SourceUpdatedAt) || !revision.SourceUpdatedAt.After(mapping.Current.Committed.SourceUpdatedAt) {
			return errors.New("revision does not supersede the pending observation")
		}
		committed := revision
		mapping.Current.Committed = &committed
		mapping.Current.Observed = nil
		mapping.Current.UpdatedAt = laterTime(mapping.Current.UpdatedAt, at)
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
		if mapping.Current.Committed != nil && *mapping.Current.Committed == revision {
			return errors.New("revision is already committed")
		}
		if existing := mapping.Current.PreparedCommit; existing != nil {
			if *existing == prepared {
				return nil
			}
			return errors.New("cannot overwrite an unresolved prepared revision")
		}
		observed := revision
		mapping.Current.Observed = &observed
		mapping.Current.PreparedCommit = &prepared
		mapping.Current.UpdatedAt = laterTime(mapping.Current.UpdatedAt, at)
		return nil
	})
}

func (store *Store) PromotePrepared(identity Identity, turnID string, at time.Time) (CommitOutcome, error) {
	if common.ValidateID(turnID) != nil {
		return CommitNotApplied, errors.New("invalid turn ID")
	}
	return store.updateAt(identity, at, func(mapping *Mapping) error {
		prepared, err := expectedPrepared(mapping, turnID)
		if err != nil {
			return err
		}
		if prepared.Phase != CommitAccepted {
			return errors.New("prepared commit has not been accepted")
		}
		promotePrepared(mapping, prepared, at)
		return nil
	})
}

// PromotePreparedIfUnchanged promotes only when the complete mapping still
// equals expected.
func (store *Store) PromotePreparedIfUnchanged(identity Identity, expected Mapping, turnID string, at time.Time) (CommitOutcome, error) {
	if common.ValidateID(turnID) != nil || expected.Validate(identity) != nil {
		return CommitNotApplied, errors.New("invalid prepared promotion precondition")
	}
	return store.updateAtExpected(identity, &expected, at, func(mapping *Mapping) error {
		prepared, err := expectedPrepared(mapping, turnID)
		if err != nil {
			return err
		}
		if prepared.Phase != CommitAccepted {
			return errors.New("prepared commit has not been accepted")
		}
		promotePrepared(mapping, prepared, at)
		return nil
	})
}

func (store *Store) MarkPreparedAccepted(identity Identity, turnID string, at time.Time) (CommitOutcome, error) {
	if common.ValidateID(turnID) != nil {
		return CommitNotApplied, errors.New("invalid turn ID")
	}
	return store.updateAt(identity, at, func(mapping *Mapping) error {
		prepared, err := expectedPrepared(mapping, turnID)
		if err != nil {
			return err
		}
		if prepared.Phase == CommitPrepared {
			prepared.Phase = CommitAccepted
		} else if prepared.Phase != CommitAccepted {
			return errors.New("invalid prepared commit phase")
		}
		mapping.Current.UpdatedAt = laterTime(mapping.Current.UpdatedAt, at)
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
			promotePrepared(mapping, prepared, at)
			return nil
		}
		if prepared.Phase != CommitPrepared {
			return errors.New("accepted prepared commit cannot be rejected")
		}
		observed := prepared.Revision
		mapping.Current.Observed = &observed
		mapping.Current.PreparedCommit = nil
		mapping.Current.UpdatedAt = laterTime(mapping.Current.UpdatedAt, at)
		return nil
	})
}

// ReconcilePreparedIfUnchanged reconciles only when the complete mapping still
// equals expected.
func (store *Store) ReconcilePreparedIfUnchanged(identity Identity, expected Mapping, turnID string, accepted bool, at time.Time) (CommitOutcome, error) {
	if common.ValidateID(turnID) != nil || expected.Validate(identity) != nil {
		return CommitNotApplied, errors.New("invalid prepared reconciliation precondition")
	}
	return store.updateAtExpected(identity, &expected, at, func(mapping *Mapping) error {
		prepared, err := expectedPrepared(mapping, turnID)
		if err != nil {
			return err
		}
		if accepted {
			promotePrepared(mapping, prepared, at)
			return nil
		}
		if prepared.Phase != CommitPrepared {
			return errors.New("accepted prepared commit cannot be rejected")
		}
		observed := prepared.Revision
		mapping.Current.Observed = &observed
		mapping.Current.PreparedCommit = nil
		mapping.Current.UpdatedAt = laterTime(mapping.Current.UpdatedAt, at)
		return nil
	})
}

func promotePrepared(mapping *Mapping, prepared *PreparedCommit, at time.Time) {
	committed := prepared.Revision
	mapping.Current.Committed = &committed
	mapping.Current.Observed = nil
	mapping.Current.PreparedCommit = nil
	mapping.Current.UpdatedAt = laterTime(mapping.Current.UpdatedAt, at)
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
	defer directory.close()
	store.ops.beforePathReturn()
	if err := store.verifyLayout(); err != nil {
		return "", err
	}
	if err := store.workspaces.verifyChild(conversationID, directory, true); err != nil {
		return "", fmt.Errorf("verify canonical workspace path: %w", err)
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
	if err := store.workspaces.removeDirectoryWithHook(conversationID, store.ops.beforeWorkspaceTombstone, store.ops.closeWorkspace, store.ops.unlinkWorkspace, store.ops.syncWorkspaces); err != nil && err != os.ErrNotExist {
		return err
	}
	return store.verifyLayout()
}

func (store *Store) EnsureProviderDirectory(name provider.Name) (string, error) {
	if !name.Valid() {
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
	defer directory.close()
	store.ops.beforePathReturn()
	if err := store.verifyLayout(); err != nil {
		return "", err
	}
	if err := store.providers.verifyChild(string(name), directory, true); err != nil {
		return "", fmt.Errorf("verify canonical provider path: %w", err)
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
	if err := store.verifyLayout(); err != nil {
		return CommitNotApplied, err
	}
	encoded, err := encodeMapping(mapping)
	if err != nil {
		return CommitNotApplied, err
	}
	temporary, temporaryName, err := store.conversations.createTemporary()
	if err != nil {
		return CommitNotApplied, err
	}
	var temporaryID fileIdentity
	defer func() {
		_ = temporary.Close()
		if temporaryID != (fileIdentity{}) {
			_ = store.conversations.removeExpected(temporaryName, temporaryID)
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
	temporaryID, err = fileIdentityForFile(temporary, 0o600)
	if err != nil {
		return CommitNotApplied, err
	}
	if err := temporary.Close(); err != nil {
		return CommitNotApplied, err
	}
	name := mappingName(key)
	if err := store.ops.publish(temporaryName, name, original, temporaryID); err != nil {
		outcome := inspectExpected(store.conversations, name, encoded)
		if outcome == CommitApplied {
			if syncErr := store.ops.syncDir(); syncErr != nil {
				return CommitUncertain, errors.Join(fmt.Errorf("publish conversation mapping: %w", err), fmt.Errorf("sync conversation directory: %w", syncErr))
			}
			if verifyErr := store.verifyLayout(); verifyErr != nil {
				return CommitUncertain, errors.Join(fmt.Errorf("publish conversation mapping: %w", err), verifyErr)
			}
		}
		return outcome, fmt.Errorf("publish conversation mapping: %w", err)
	}
	if err := store.ops.syncDir(); err != nil {
		return CommitUncertain, fmt.Errorf("sync conversation directory: %w", err)
	}
	if err := store.verifyLayout(); err != nil {
		return CommitUncertain, err
	}
	return CommitApplied, nil
}

func (store *Store) removeLocked(key string, original fileIdentity) (CommitOutcome, error) {
	if err := store.verifyLayout(); err != nil {
		return CommitNotApplied, err
	}
	name := mappingName(key)
	if err := store.ops.removeExpected(name, original); err != nil {
		if _, inspectErr := store.conversations.targetIdentity(name); errors.Is(inspectErr, os.ErrNotExist) {
			if syncErr := store.ops.syncDir(); syncErr != nil {
				return CommitUncertain, errors.Join(fmt.Errorf("remove conversation mapping: %w", err), fmt.Errorf("sync conversation directory: %w", syncErr))
			}
			if verifyErr := store.verifyLayout(); verifyErr != nil {
				return CommitUncertain, errors.Join(fmt.Errorf("remove conversation mapping: %w", err), verifyErr)
			}
			return CommitApplied, fmt.Errorf("remove conversation mapping: %w", err)
		} else if inspectErr != nil {
			return CommitUncertain, fmt.Errorf("remove conversation mapping: %w", err)
		}
		return CommitNotApplied, fmt.Errorf("remove conversation mapping: %w", err)
	}
	if err := store.ops.syncDir(); err != nil {
		return CommitUncertain, fmt.Errorf("sync conversation directory: %w", err)
	}
	if err := store.verifyLayout(); err != nil {
		return CommitUncertain, err
	}
	return CommitApplied, nil
}

func (store *Store) verifyLayout() error {
	if store.invalidLayout.Load() {
		return errors.New("agent state canonical layout previously changed")
	}
	if err := store.layout.verify(); err != nil {
		store.invalidLayout.Store(true)
		return fmt.Errorf("agent state canonical layout changed: %w", err)
	}
	return nil
}

func (store *Store) begin(key string) (func(), error) {
	store.lifecycle.RLock()
	if store.closed {
		store.lifecycle.RUnlock()
		return nil, ErrClosed
	}
	if err := store.verifyLayout(); err != nil {
		store.lifecycle.RUnlock()
		return nil, err
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

type sessionSnapshot struct {
	updatedAt time.Time
	committed *Revision
	observed  *Revision
	prepared  *PreparedCommit
}

func snapshotSessions(mapping Mapping) map[string]sessionSnapshot {
	snapshots := make(map[string]sessionSnapshot, len(mapping.Archives)+1)
	add := func(session Session) {
		snapshot := sessionSnapshot{updatedAt: session.UpdatedAt}
		if session.Committed != nil {
			committed := *session.Committed
			snapshot.committed = &committed
		}
		if session.Observed != nil {
			observed := *session.Observed
			snapshot.observed = &observed
		}
		if session.PreparedCommit != nil {
			prepared := *session.PreparedCommit
			snapshot.prepared = &prepared
		}
		snapshots[session.ConversationID] = snapshot
	}
	if mapping.Current != nil {
		add(*mapping.Current)
	}
	for _, session := range mapping.Archives {
		add(session)
	}
	return snapshots
}

func clampSessionTimes(mapping *Mapping, before map[string]sessionSnapshot) {
	clamp := func(session *Session) {
		if snapshot, exists := before[session.ConversationID]; exists {
			session.UpdatedAt = laterTime(snapshot.updatedAt, session.UpdatedAt)
		}
	}
	if mapping.Current != nil {
		clamp(mapping.Current)
	}
	for index := range mapping.Archives {
		clamp(&mapping.Archives[index])
	}
}

func validateSessionTransitions(mapping Mapping, before map[string]sessionSnapshot) error {
	after := make(map[string]Session, len(mapping.Archives)+1)
	if mapping.Current != nil {
		after[mapping.Current.ConversationID] = *mapping.Current
	}
	for _, session := range mapping.Archives {
		after[session.ConversationID] = session
	}
	for id, previous := range before {
		current, exists := after[id]
		if !exists {
			if previous.prepared != nil {
				return errors.New("cannot remove a session with an unresolved prepared revision")
			}
			continue
		}
		if previous.prepared != nil {
			switch {
			case current.PreparedCommit != nil:
				unchanged := *current.PreparedCommit == *previous.prepared
				accepted := previous.prepared.Phase == CommitPrepared && current.PreparedCommit.Phase == CommitAccepted && current.PreparedCommit.Revision == previous.prepared.Revision && current.PreparedCommit.TurnID == previous.prepared.TurnID
				if !unchanged && !accepted {
					return errors.New("cannot overwrite or downgrade an unresolved prepared revision")
				}
			case current.Committed != nil && *current.Committed == previous.prepared.Revision && current.Observed == nil:
				// Provider acceptance proof or promotion resolved the record.
			case previous.prepared.Phase == CommitPrepared && current.Observed != nil && *current.Observed == previous.prepared.Revision:
				// A provider rejection may clear only an unaccepted prepared record.
			default:
				return errors.New("cannot discard an unresolved prepared revision")
			}
		}
		if !equalRevisionPointers(previous.committed, current.Committed) {
			if current.Committed == nil {
				return errors.New("cannot discard a committed revision")
			}
			resolvedPrepared := previous.prepared != nil && current.PreparedCommit == nil && current.Observed == nil && *current.Committed == previous.prepared.Revision
			acknowledgedCommitted := previous.prepared == nil && previous.committed != nil && previous.observed != nil && current.PreparedCommit == nil && current.Observed == nil &&
				current.Committed.Digest == previous.committed.Digest && current.Committed.Revision == RevisionReplacement &&
				current.Committed.SourceUpdatedAt.After(previous.observed.SourceUpdatedAt) && current.Committed.SourceUpdatedAt.After(previous.committed.SourceUpdatedAt)
			if !resolvedPrepared && !acknowledgedCommitted {
				return errors.New("cannot change a committed revision without resolving its prepared record")
			}
		}
	}
	return nil
}

func equalRevisionPointers(left, right *Revision) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func laterTime(left, right time.Time) time.Time {
	if right.Before(left) {
		return left
	}
	return right
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
