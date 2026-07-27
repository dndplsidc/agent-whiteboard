//go:build unix

package pi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/provider"
)

const maxNativeRecordBytes = 64 << 10

type nativeManager struct {
	root     string
	sessions string
	ids      common.IDGenerator
	clock    common.Clock
	syncDir  func(string) error
}

type nativeFileIdentity struct{ device, inode uint64 }

type nativeAllocation struct {
	Ref       provider.NativeSessionRef
	Workspace string
	path      string
	identity  nativeFileIdentity
	createdAt time.Time
}

type nativeMetadata struct {
	Schema    int       `json:"schema"`
	Ref       string    `json:"ref"`
	SessionID string    `json:"sessionId"`
	Model     string    `json:"model"`
	Workspace string    `json:"workspace"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type piSessionHeader struct {
	Type      string      `json:"type"`
	Version   json.Number `json:"version"`
	ID        string      `json:"id"`
	Timestamp string      `json:"timestamp"`
	CWD       string      `json:"cwd"`
}

func newNativeManager(root string, ids common.IDGenerator, clock common.Clock) (*nativeManager, error) {
	if ids == nil || clock == nil || !validCanonicalPath(root) {
		return nil, errors.New("invalid Pi native manager configuration")
	}
	if err := verifyDirectory(root, 0o700); err != nil {
		return nil, errors.New("invalid Pi provider directory")
	}
	sessions := filepath.Join(root, "sessions")
	if err := os.Mkdir(sessions, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, errors.New("create Pi sessions directory")
	}
	if err := verifyDirectory(sessions, 0o700); err != nil {
		return nil, errors.New("invalid Pi sessions directory")
	}
	if err := syncDirectory(root); err != nil {
		return nil, errors.New("sync Pi provider directory")
	}
	return &nativeManager{root: root, sessions: sessions, ids: ids, clock: clock, syncDir: syncDirectory}, nil
}

func (manager *nativeManager) allocate(workspace string) (nativeAllocation, error) {
	if manager == nil || !validCanonicalPath(workspace) || manager.verify() != nil {
		return nativeAllocation{}, errors.New("invalid Pi native allocation")
	}
	value, err := manager.ids.NewID()
	if err != nil || common.ValidateID(value) != nil {
		return nativeAllocation{}, errors.New("allocate Pi native reference")
	}
	ref, err := provider.NewNativeSessionRef(value)
	if err != nil {
		return nativeAllocation{}, errors.New("allocate Pi native reference")
	}
	createdAt := manager.clock.Now().UTC()
	if createdAt.IsZero() {
		return nativeAllocation{}, errors.New("invalid Pi native allocation time")
	}
	path := filepath.Join(manager.sessions, value+".jsonl")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nativeAllocation{}, errors.New("create Pi native session")
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil || closeErr != nil || !secureRegular(info) {
		_ = os.Remove(path)
		return nativeAllocation{}, errors.New("create Pi native session")
	}
	if err := manager.syncDir(manager.sessions); err != nil {
		_ = os.Remove(path)
		return nativeAllocation{}, errors.New("sync Pi native session")
	}
	return nativeAllocation{Ref: ref, Workspace: workspace, path: path, identity: identityOf(info), createdAt: createdAt}, nil
}

func (manager *nativeManager) finalizeAllocation(allocation nativeAllocation, state startupState) (provider.NativeSession, error) {
	if manager == nil || manager.verifyAllocation(allocation) != nil || !state.valid() || state.SessionFile != allocation.path || state.Workspace != allocation.Workspace || state.SessionID == "" {
		return provider.NativeSession{}, errors.New("invalid Pi native allocation finalization")
	}
	if err := validateSessionFile(allocation.path, state.SessionID, allocation.Workspace); err != nil {
		return provider.NativeSession{}, provider.NewProviderError(provider.ErrorNativeSessionMissing)
	}
	metadataPath := manager.metadataPath(allocation.Ref)
	updated := manager.clock.Now().UTC()
	if updated.Before(allocation.createdAt) {
		updated = allocation.createdAt
	}
	metadata := nativeMetadata{Schema: 1, Ref: allocation.Ref.Value(), SessionID: state.SessionID, Model: state.Model, Workspace: allocation.Workspace, CreatedAt: allocation.createdAt, UpdatedAt: updated}
	if _, err := os.Lstat(metadataPath); err == nil {
		existing, readErr := manager.readMetadata(allocation.Ref)
		if readErr != nil || !metadataMatchesFinalization(existing, metadata) {
			return provider.NativeSession{}, errors.New("Pi native metadata already exists")
		}
		if manager.syncDir(manager.sessions) != nil {
			return provider.NativeSession{}, errors.New("sync Pi native metadata")
		}
		return existing.native(allocation.Ref), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return provider.NativeSession{}, errors.New("inspect Pi native metadata")
	}
	if err := manager.writeMetadata(metadataPath, metadata); err != nil {
		// Publication may have won concurrently or become visible before an
		// ambiguous directory-sync failure. Revalidate the exact winner and
		// re-establish directory durability before reporting success.
		existing, readErr := manager.readMetadata(allocation.Ref)
		if readErr != nil || !metadataMatchesFinalization(existing, metadata) || manager.syncDir(manager.sessions) != nil {
			return provider.NativeSession{}, err
		}
		return existing.native(allocation.Ref), nil
	}
	return metadata.native(allocation.Ref), nil
}

func (manager *nativeManager) rollbackAllocation(allocation nativeAllocation) error {
	if manager == nil || manager.verifyAllocation(allocation) != nil {
		return errors.New("invalid Pi native allocation rollback")
	}
	if _, err := os.Lstat(manager.metadataPath(allocation.Ref)); err == nil || !errors.Is(err, os.ErrNotExist) {
		return errors.New("Pi native allocation is finalized")
	}
	if err := os.Remove(allocation.path); err != nil {
		return errors.New("rollback Pi native allocation")
	}
	if err := manager.syncDir(manager.sessions); err != nil {
		return errors.New("sync Pi native allocation rollback")
	}
	return nil
}

func (manager *nativeManager) inspect(ref provider.NativeSessionRef) (provider.NativeSession, error) {
	if manager == nil || manager.verify() != nil || !validNativeRef(ref) {
		return provider.NativeSession{}, provider.NewProviderError(provider.ErrorNativeSessionMissing)
	}
	metadata, err := manager.readMetadata(ref)
	if err != nil || validateSessionFile(manager.sessionPath(ref), metadata.SessionID, metadata.Workspace) != nil {
		return provider.NativeSession{}, provider.NewProviderError(provider.ErrorNativeSessionMissing)
	}
	return metadata.native(ref), nil
}

func (manager *nativeManager) delete(ref provider.NativeSessionRef) error {
	if manager == nil || manager.verify() != nil || !validNativeRef(ref) {
		return provider.NewProviderError(provider.ErrorNativeSessionMissing)
	}
	sessionPath, metadataPath := manager.sessionPath(ref), manager.metadataPath(ref)
	sessionInfo, sessionErr := os.Lstat(sessionPath)
	metadataInfo, metadataErr := os.Lstat(metadataPath)
	sessionMissing, metadataMissing := errors.Is(sessionErr, os.ErrNotExist), errors.Is(metadataErr, os.ErrNotExist)
	if sessionMissing && metadataMissing {
		return nil
	}
	if metadataMissing {
		return provider.NewProviderError(provider.ErrorNativeSessionMissing)
	}
	if metadataErr != nil || !secureRegular(metadataInfo) {
		return provider.NewProviderError(provider.ErrorNativeSessionMissing)
	}
	metadata, err := manager.readMetadata(ref)
	if err != nil {
		return provider.NewProviderError(provider.ErrorNativeSessionMissing)
	}
	if !sessionMissing {
		if sessionErr != nil || !secureRegular(sessionInfo) || validateSessionFile(sessionPath, metadata.SessionID, metadata.Workspace) != nil {
			return provider.NewProviderError(provider.ErrorNativeSessionMissing)
		}
		if err := os.Remove(sessionPath); err != nil || manager.syncDir(manager.sessions) != nil {
			return provider.NewProviderError(provider.ErrorNativeSessionMissing)
		}
	}
	if err := os.Remove(metadataPath); err != nil || manager.syncDir(manager.sessions) != nil {
		return provider.NewProviderError(provider.ErrorNativeSessionMissing)
	}
	return nil
}

func (manager *nativeManager) verify() error {
	if !validCanonicalPath(manager.root) || manager.sessions != filepath.Join(manager.root, "sessions") || verifyDirectory(manager.root, 0o700) != nil || verifyDirectory(manager.sessions, 0o700) != nil {
		return errors.New("invalid Pi native directory")
	}
	return nil
}

func (manager *nativeManager) verifyAllocation(allocation nativeAllocation) error {
	if manager.verify() != nil || !validNativeRef(allocation.Ref) || allocation.path != manager.sessionPath(allocation.Ref) || !validCanonicalPath(allocation.Workspace) || allocation.createdAt.IsZero() || allocation.createdAt.Location() != time.UTC {
		return errors.New("invalid Pi native allocation")
	}
	info, err := os.Lstat(allocation.path)
	if err != nil || !secureRegular(info) || identityOf(info) != allocation.identity {
		return errors.New("invalid Pi native allocation identity")
	}
	return nil
}

func (manager *nativeManager) sessionPath(ref provider.NativeSessionRef) string {
	return filepath.Join(manager.sessions, ref.Value()+".jsonl")
}
func (manager *nativeManager) metadataPath(ref provider.NativeSessionRef) string {
	return filepath.Join(manager.sessions, ref.Value()+".json")
}

func (manager *nativeManager) writeMetadata(path string, metadata nativeMetadata) error {
	encoded, err := json.Marshal(metadata)
	if err != nil || len(encoded) > maxNativeRecordBytes {
		return errors.New("encode Pi native metadata")
	}
	temporary, err := os.CreateTemp(manager.sessions, ".metadata-*")
	if err != nil {
		return errors.New("create Pi native metadata")
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return errors.New("secure Pi native metadata")
	}
	if err := writeFull(temporary, encoded); err != nil || temporary.Sync() != nil || temporary.Close() != nil {
		_ = temporary.Close()
		return errors.New("write Pi native metadata")
	}
	// Link publishes without replacing a sidecar created after the existence
	// check. The temporary and final names refer to the same fsynced inode.
	if err := os.Link(temporaryPath, path); err != nil {
		return errors.New("commit Pi native metadata")
	}
	if err := manager.syncDir(manager.sessions); err != nil {
		return errors.New("sync Pi native metadata")
	}
	if err := os.Remove(temporaryPath); err != nil {
		return errors.New("remove Pi native metadata temporary")
	}
	if err := manager.syncDir(manager.sessions); err != nil {
		return errors.New("sync Pi native metadata")
	}
	return nil
}

func metadataMatchesFinalization(existing, expected nativeMetadata) bool {
	return existing.Schema == expected.Schema && existing.Ref == expected.Ref && existing.SessionID == expected.SessionID && existing.Model == expected.Model && existing.Workspace == expected.Workspace && existing.CreatedAt.Equal(expected.CreatedAt)
}

func (manager *nativeManager) readMetadata(ref provider.NativeSessionRef) (nativeMetadata, error) {
	path := manager.metadataPath(ref)
	info, err := os.Lstat(path)
	if err != nil || !secureRegular(info) || info.Size() <= 0 || info.Size() > maxNativeRecordBytes {
		return nativeMetadata{}, errors.New("invalid Pi native metadata file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nativeMetadata{}, errors.New("read Pi native metadata")
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !secureRegular(openedInfo) || identityOf(openedInfo) != identityOf(info) {
		return nativeMetadata{}, errors.New("invalid Pi native metadata identity")
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxNativeRecordBytes+1))
	decoder.DisallowUnknownFields()
	var metadata nativeMetadata
	if decoder.Decode(&metadata) != nil || decoder.Decode(&struct{}{}) != io.EOF || !validMetadata(metadata, ref) {
		return nativeMetadata{}, errors.New("invalid Pi native metadata")
	}
	return metadata, nil
}

func validMetadata(metadata nativeMetadata, ref provider.NativeSessionRef) bool {
	if metadata.Schema != 1 || metadata.Ref != ref.Value() || !validStartupText(metadata.SessionID, provider.MaxNativeReferenceBytes) || metadata.Model == "" || len(metadata.Model) > provider.MaxTitleBytes || !validCanonicalPath(metadata.Workspace) || metadata.CreatedAt.IsZero() || metadata.UpdatedAt.IsZero() || metadata.CreatedAt.Location() != time.UTC || metadata.UpdatedAt.Location() != time.UTC || metadata.UpdatedAt.Before(metadata.CreatedAt) {
		return false
	}
	return metadata.native(ref).Validate() == nil
}

func (metadata nativeMetadata) native(ref provider.NativeSessionRef) provider.NativeSession {
	return provider.NativeSession{Ref: ref, Provider: provider.NamePi, Model: metadata.Model, CreatedAt: metadata.CreatedAt, UpdatedAt: metadata.UpdatedAt}
}

func validateSessionFile(path, sessionID, workspace string) error {
	if !validCanonicalPath(path) || !validCanonicalPath(workspace) || !validStartupText(sessionID, provider.MaxNativeReferenceBytes) {
		return errors.New("invalid Pi session identity")
	}
	info, err := os.Lstat(path)
	if err != nil || !secureRegular(info) || info.Size() <= 0 {
		return errors.New("invalid Pi session file")
	}
	file, err := os.Open(path)
	if err != nil {
		return errors.New("read Pi session header")
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !secureRegular(openedInfo) || identityOf(openedInfo) != identityOf(info) {
		return errors.New("invalid Pi session identity")
	}
	reader := bufio.NewReader(io.LimitReader(file, maxNativeRecordBytes+1))
	line, err := reader.ReadBytes('\n')
	if err != nil || len(line) <= 1 || len(line) > maxNativeRecordBytes || line[len(line)-1] != '\n' {
		return errors.New("invalid Pi session header")
	}
	line = line[:len(line)-1]
	if len(line) == 0 || line[len(line)-1] == '\r' || validateJSONObject(line) != nil {
		return errors.New("invalid Pi session header")
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var header piSessionHeader
	if decoder.Decode(&header) != nil || decoder.Decode(&struct{}{}) != io.EOF || header.Type != "session" || header.ID != sessionID || header.CWD != workspace || header.Version == "" {
		return errors.New("invalid Pi session header")
	}
	version, err := header.Version.Int64()
	if err != nil || version != 3 {
		return errors.New("invalid Pi session version")
	}
	timestamp, err := time.Parse(time.RFC3339Nano, header.Timestamp)
	if err != nil || timestamp.Location() != time.UTC {
		return errors.New("invalid Pi session timestamp")
	}
	if err := file.Sync(); err != nil {
		return errors.New("sync Pi session header")
	}
	return nil
}

func validNativeRef(ref provider.NativeSessionRef) bool {
	return ref.Valid() && common.ValidateID(ref.Value()) == nil
}

func validCanonicalPath(path string) bool {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > provider.MaxNativeReferenceBytes {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && resolved == path
}

func verifyDirectory(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return errors.New("insecure directory")
	}
	return nil
}

func secureRegular(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode().IsRegular() && info.Mode().Perm() == 0o600 && stat.Uid == uint32(os.Geteuid())
}

func identityOf(info os.FileInfo) nativeFileIdentity {
	stat := info.Sys().(*syscall.Stat_t)
	return nativeFileIdentity{device: uint64(stat.Dev), inode: uint64(stat.Ino)}
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
