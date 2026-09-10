package catalog

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/dndplsidc/agent-whiteboard/internal/common"
	"github.com/dndplsidc/agent-whiteboard/internal/config"
)

const (
	SchemaVersion = 1
	directoryMode = 0o700
	fileMode      = 0o600
)

var serverKeyPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Kind string

const (
	KindMarkdown Kind = "markdown"
	KindHTML     Kind = "html"
)

type State string

const (
	StateCreated           State = "created"
	StateCreationUncertain State = "creation_uncertain"
	StateDeleted           State = "deleted"
)

type Identity struct {
	Server string
	Kind   Kind
	ID     string
}

type Record struct {
	SchemaVersion  int    `json:"schema_version"`
	Server         string `json:"server"`
	Kind           Kind   `json:"kind"`
	ID             string `json:"id"`
	URL            string `json:"url"`
	Title          string `json:"title"`
	Summary        string `json:"summary"`
	SourceFilename string `json:"source_filename"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
	ExpiresAt      *int64 `json:"expires_at"`
	Permanent      bool   `json:"permanent"`
	State          State  `json:"state"`
	DeletedAt      *int64 `json:"deleted_at"`
}

func (record Record) Identity() Identity {
	return Identity{Server: record.Server, Kind: record.Kind, ID: record.ID}
}

type ResultRecord struct {
	Record
	Expired bool `json:"expired"`
}

type Query struct {
	Kind   Kind
	Terms  string
	Limit  int
	Offset int
}

type Page struct {
	Records []ResultRecord
	Total   int
	Limit   int
	Offset  int
}

type Config struct {
	Root  string
	Clock common.Clock
}

type Creation struct {
	Server         string
	Kind           Kind
	ID             string
	URL            string
	Title          string
	Summary        string
	SourceFilename string
	ExpiresAt      *int64
	Permanent      bool
	State          State
}

type Update struct {
	URL            string
	SourceFilename string
	Title          *string
	Summary        *string
	ExpiresAt      *int64
	Permanent      bool
}

type Store struct {
	rootPath string
	clock    common.Clock
}

type Entry struct {
	root     *os.Root
	lock     *fileLock
	record   Record
	identity Identity
	clock    common.Clock
	closed   bool
}

func New(settings Config) (*Store, error) {
	if settings.Root == "" {
		return nil, errors.New("catalog root is required")
	}
	if common.IsNil(settings.Clock) {
		return nil, errors.New("catalog clock is required")
	}
	absolute, err := filepath.Abs(settings.Root)
	if err != nil || absolute == string(filepath.Separator) {
		return nil, errors.New("invalid catalog root")
	}
	return &Store{rootPath: filepath.Clean(absolute), clock: settings.Clock}, nil
}

func ServerKey(server string) string {
	digest := sha256.Sum256([]byte(server))
	return hex.EncodeToString(digest[:])
}

func (store *Store) Prepare(ctx context.Context, server string, kind Kind) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateLocation(server, kind); err != nil {
		return err
	}
	if err := prepareCatalogRoot(store.rootPath, syncParentDirectory); err != nil {
		return err
	}
	root, err := openPrivateRoot(store.rootPath)
	if err != nil {
		return fmt.Errorf("open catalog: %w", err)
	}
	defer root.Close()
	entries, err := ensurePrivateRoot(root, "entries")
	if err != nil {
		return fmt.Errorf("prepare catalog entries: %w", err)
	}
	serverRoot, err := ensurePrivateRoot(entries, ServerKey(server))
	_ = entries.Close()
	if err != nil {
		return fmt.Errorf("prepare catalog server: %w", err)
	}
	kindRoot, err := ensurePrivateRoot(serverRoot, string(kind))
	_ = serverRoot.Close()
	if err != nil {
		return fmt.Errorf("prepare catalog kind: %w", err)
	}
	defer kindRoot.Close()
	if err := checkWritable(kindRoot); err != nil {
		return err
	}
	return ctx.Err()
}

func checkWritable(root *os.Root) error {
	name, file, err := createTemporary(root, ".preflight")
	if err != nil {
		return fmt.Errorf("test catalog write: %w", err)
	}
	if closeErr := file.Close(); closeErr != nil {
		_ = root.Remove(name)
		return fmt.Errorf("test catalog write: %w", closeErr)
	}
	if err := root.Remove(name); err != nil {
		return fmt.Errorf("test catalog write: %w", err)
	}
	if err := syncDirectory(root); err != nil {
		return fmt.Errorf("test catalog write: %w", err)
	}
	return nil
}

func (store *Store) SaveCreated(ctx context.Context, record Record) error {
	if err := validateRecord(record); err != nil {
		return err
	}
	if err := store.Prepare(ctx, record.Server, record.Kind); err != nil {
		return err
	}
	root, err := store.openKindRoot(record.Identity(), true)
	if err != nil {
		return err
	}
	lock, err := acquireFileLock(ctx, root, record.ID+".lock")
	if err != nil {
		_ = root.Close()
		return fmt.Errorf("lock catalog record: %w", err)
	}
	defer lock.release()
	defer root.Close()
	return writeRecord(root, record)
}

func (store *Store) RecordCreation(ctx context.Context, creation Creation) error {
	now := store.clock.Now().UTC().Unix()
	return store.SaveCreated(ctx, Record{
		SchemaVersion: SchemaVersion,
		Server:        creation.Server, Kind: creation.Kind, ID: creation.ID, URL: creation.URL,
		Title: creation.Title, Summary: creation.Summary, SourceFilename: creation.SourceFilename,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: creation.ExpiresAt, Permanent: creation.Permanent,
		State: creation.State,
	})
}

func (store *Store) OpenExisting(ctx context.Context, identity Identity) (*Entry, bool, error) {
	if err := validateIdentity(identity); err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	root, err := store.openKindRoot(identity, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	name := identity.ID + ".json"
	if _, err := root.Lstat(name); errors.Is(err, os.ErrNotExist) {
		_ = root.Close()
		return nil, false, nil
	} else if err != nil {
		_ = root.Close()
		return nil, false, fmt.Errorf("inspect catalog record: %w", err)
	}
	lock, err := acquireFileLock(ctx, root, identity.ID+".lock")
	if err != nil {
		_ = root.Close()
		return nil, false, fmt.Errorf("lock catalog record: %w", err)
	}
	record, err := readRecord(root, name, identity)
	if err == nil {
		err = checkWritable(root)
	}
	if err != nil {
		lock.release()
		_ = root.Close()
		return nil, false, err
	}
	return &Entry{root: root, lock: lock, record: record, identity: identity, clock: store.clock}, true, nil
}

func (entry *Entry) Record() Record { return entry.record }

func (entry *Entry) Save(ctx context.Context, record Record) error {
	if entry == nil || entry.closed || entry.root == nil || entry.lock == nil {
		return errors.New("catalog entry is closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if record.Identity() != entry.identity {
		return errors.New("catalog record identity cannot change")
	}
	if err := validateRecord(record); err != nil {
		return err
	}
	if err := writeRecord(entry.root, record); err != nil {
		return err
	}
	entry.record = record
	return nil
}

func (entry *Entry) Update(ctx context.Context, update Update) error {
	record := entry.Record()
	record.URL = update.URL
	record.SourceFilename = update.SourceFilename
	if update.Title != nil {
		record.Title = *update.Title
	}
	if update.Summary != nil {
		record.Summary = *update.Summary
	}
	record.UpdatedAt = entry.clock.Now().UTC().Unix()
	record.ExpiresAt = update.ExpiresAt
	record.Permanent = update.Permanent
	record.State = StateCreated
	record.DeletedAt = nil
	return entry.Save(ctx, record)
}

func (entry *Entry) MarkDeleted(ctx context.Context) error {
	record := entry.Record()
	now := entry.clock.Now().UTC().Unix()
	record.UpdatedAt = now
	record.State = StateDeleted
	record.DeletedAt = &now
	return entry.Save(ctx, record)
}

func (entry *Entry) Close() error {
	if entry == nil || entry.closed {
		return nil
	}
	entry.closed = true
	entry.lock.release()
	return entry.root.Close()
}

func (store *Store) List(ctx context.Context, query Query) (Page, error) {
	if err := validateQuery(query); err != nil {
		return Page{}, err
	}
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	root, err := openPrivateRoot(store.rootPath)
	if errors.Is(err, os.ErrNotExist) {
		return Page{Records: []ResultRecord{}, Limit: query.Limit, Offset: query.Offset}, nil
	}
	if err != nil {
		return Page{}, fmt.Errorf("open catalog: %w", err)
	}
	defer root.Close()
	entries, err := openPrivateChild(root, "entries")
	if errors.Is(err, os.ErrNotExist) {
		return Page{Records: []ResultRecord{}, Limit: query.Limit, Offset: query.Offset}, nil
	}
	if err != nil {
		return Page{}, fmt.Errorf("open catalog entries: %w", err)
	}
	defer entries.Close()

	records, err := scanRecords(ctx, entries)
	if err != nil {
		return Page{}, err
	}
	terms := strings.Fields(strings.ToLower(query.Terms))
	filtered := make([]Record, 0, len(records))
	for _, record := range records {
		if query.Kind != "" && record.Kind != query.Kind {
			continue
		}
		haystack := strings.ToLower(record.Title + "\n" + record.Summary)
		matched := true
		for _, term := range terms {
			if !strings.Contains(haystack, term) {
				matched = false
				break
			}
		}
		if matched {
			filtered = append(filtered, record)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].CreatedAt != filtered[j].CreatedAt {
			return filtered[i].CreatedAt > filtered[j].CreatedAt
		}
		left, right := filtered[i], filtered[j]
		if left.Server != right.Server {
			return left.Server < right.Server
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		return left.ID < right.ID
	})
	total := len(filtered)
	start := min(query.Offset, total)
	end := start + min(query.Limit, total-start)
	now := store.clock.Now().Unix()
	pageRecords := make([]ResultRecord, 0, end-start)
	for _, record := range filtered[start:end] {
		pageRecords = append(pageRecords, ResultRecord{Record: record, Expired: record.ExpiresAt != nil && *record.ExpiresAt <= now})
	}
	return Page{Records: pageRecords, Total: total, Limit: query.Limit, Offset: query.Offset}, nil
}

func scanRecords(ctx context.Context, entries *os.Root) ([]Record, error) {
	serverNames, err := directoryNames(entries)
	if err != nil {
		return nil, fmt.Errorf("read catalog entries: %w", err)
	}
	var records []Record
	for _, serverName := range serverNames {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !serverKeyPattern.MatchString(serverName) {
			return nil, fmt.Errorf("read catalog entries: invalid server directory %q", serverName)
		}
		serverRoot, err := openPrivateChild(entries, serverName)
		if err != nil {
			return nil, fmt.Errorf("read catalog server %q: %w", serverName, err)
		}
		kindNames, err := directoryNames(serverRoot)
		if err != nil {
			_ = serverRoot.Close()
			return nil, fmt.Errorf("read catalog server %q: %w", serverName, err)
		}
		for _, kindName := range kindNames {
			kind := Kind(kindName)
			if !kind.valid() {
				_ = serverRoot.Close()
				return nil, fmt.Errorf("read catalog server %q: invalid kind directory %q", serverName, kindName)
			}
			kindRoot, err := openPrivateChild(serverRoot, kindName)
			if err != nil {
				_ = serverRoot.Close()
				return nil, fmt.Errorf("read catalog kind %q: %w", kindName, err)
			}
			names, err := directoryNames(kindRoot)
			if err != nil {
				_ = kindRoot.Close()
				_ = serverRoot.Close()
				return nil, fmt.Errorf("read catalog kind %q: %w", kindName, err)
			}
			for _, name := range names {
				if strings.HasSuffix(name, ".lock") || strings.HasPrefix(name, ".") {
					continue
				}
				id, ok := strings.CutSuffix(name, ".json")
				if !ok || common.ValidateID(id) != nil {
					_ = kindRoot.Close()
					_ = serverRoot.Close()
					return nil, fmt.Errorf("read catalog kind %q: invalid record filename %q", kindName, name)
				}
				record, err := readRecord(kindRoot, name, Identity{Kind: kind, ID: id})
				if err != nil {
					_ = kindRoot.Close()
					_ = serverRoot.Close()
					return nil, err
				}
				if ServerKey(record.Server) != serverName {
					_ = kindRoot.Close()
					_ = serverRoot.Close()
					return nil, fmt.Errorf("read catalog record %q: server identity does not match path", name)
				}
				records = append(records, record)
			}
			_ = kindRoot.Close()
		}
		_ = serverRoot.Close()
	}
	if records == nil {
		records = []Record{}
	}
	return records, nil
}

func (store *Store) openKindRoot(identity Identity, create bool) (*os.Root, error) {
	root, err := openPrivateRoot(store.rootPath)
	if err != nil {
		return nil, err
	}
	entries, err := openOrEnsureChild(root, "entries", create)
	_ = root.Close()
	if err != nil {
		return nil, err
	}
	server, err := openOrEnsureChild(entries, ServerKey(identity.Server), create)
	_ = entries.Close()
	if err != nil {
		return nil, err
	}
	kind, err := openOrEnsureChild(server, string(identity.Kind), create)
	_ = server.Close()
	return kind, err
}

func openOrEnsureChild(parent *os.Root, name string, create bool) (*os.Root, error) {
	if create {
		return ensurePrivateRoot(parent, name)
	}
	return openPrivateChild(parent, name)
}

func prepareCatalogRoot(path string, syncParent func(string) error) error {
	if err := ensureParentDirectory(filepath.Dir(path), syncParent); err != nil {
		return fmt.Errorf("prepare catalog parent: %w", err)
	}
	if err := ensurePrivateDirectory(path, syncParent); err != nil {
		return fmt.Errorf("prepare catalog: %w", err)
	}
	return nil
}

func syncParentDirectory(path string) error {
	parent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	return syncDirectory(parent)
}

func ensurePrivateDirectory(path string, syncParent func(string) error) error {
	created := false
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if mkdirErr := os.Mkdir(path, directoryMode); mkdirErr != nil {
			if !errors.Is(mkdirErr, os.ErrExist) {
				return mkdirErr
			}
		} else {
			created = true
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if !privateDirectory(info) {
		return errors.New("path must be a private directory and not a symbolic link")
	}
	if created {
		root, openErr := os.OpenRoot(path)
		if openErr != nil {
			return openErr
		}
		opened, inspectErr := root.Stat(".")
		if inspectErr != nil || !os.SameFile(info, opened) {
			_ = root.Close()
			return errors.New("catalog directory changed while opening")
		}
		chmodErr := chmodRootDirectory(root)
		closeErr := root.Close()
		if err := errors.Join(chmodErr, closeErr); err != nil {
			return err
		}
	}
	// An existing directory may have been left by a failed sync or a concurrent
	// creator. Establish its parent edge before claiming catalog readiness.
	return syncParent(path)
}

func ensureParentDirectory(path string, syncParent func(string) error) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, directoryMode); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return err
			}
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("catalog parent must be a directory and not a symbolic link")
	}
	return syncParent(path)
}

func openPrivateRoot(path string) (*os.Root, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !privateDirectory(before) {
		return nil, errors.New("catalog path must be a private directory and not a symbolic link")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !privateDirectory(opened) || !os.SameFile(before, opened) {
		_ = root.Close()
		return nil, errors.New("catalog directory changed while opening")
	}
	return root, nil
}

func ensurePrivateRoot(parent *os.Root, name string) (*os.Root, error) {
	created := false
	info, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		if mkdirErr := parent.Mkdir(name, directoryMode); mkdirErr != nil {
			if !errors.Is(mkdirErr, os.ErrExist) {
				return nil, mkdirErr
			}
		} else {
			created = true
		}
		info, err = parent.Lstat(name)
	}
	if err != nil {
		return nil, err
	}
	if !privateDirectory(info) {
		return nil, errors.New("catalog component must be a private directory and not a symbolic link")
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := child.Stat(".")
	if err != nil || !privateDirectory(opened) || !os.SameFile(info, opened) {
		_ = child.Close()
		return nil, errors.New("catalog component changed while opening")
	}
	if created {
		if err := chmodRootDirectory(child); err != nil {
			_ = child.Close()
			return nil, err
		}
		opened, err = child.Stat(".")
	}
	if err != nil || !privateDirectory(opened) || created && opened.Mode().Perm() != directoryMode || !os.SameFile(info, opened) {
		_ = child.Close()
		return nil, errors.New("catalog component changed while opening")
	}
	if err := syncDirectory(parent); err != nil {
		_ = child.Close()
		return nil, err
	}
	return child, nil
}

func openPrivateChild(parent *os.Root, name string) (*os.Root, error) {
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !privateDirectory(info) {
		return nil, errors.New("catalog component must be a private directory and not a symbolic link")
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	opened, err := child.Stat(".")
	if err != nil || !privateDirectory(opened) || !os.SameFile(info, opened) {
		_ = child.Close()
		return nil, errors.New("catalog component changed while opening")
	}
	return child, nil
}

func privateDirectory(info os.FileInfo) bool {
	return info != nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o077 == 0
}

func writeRecord(root *os.Root, record Record) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode catalog record: %w", err)
	}
	encoded = append(encoded, '\n')
	temporary, file, err := createTemporary(root, "."+record.ID+".json")
	if err != nil {
		return fmt.Errorf("create catalog record: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = root.Remove(temporary)
		}
	}()
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		return fmt.Errorf("write catalog record: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync catalog record: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close catalog record: %w", err)
	}
	if err := root.Rename(temporary, record.ID+".json"); err != nil {
		return fmt.Errorf("publish catalog record: %w", err)
	}
	cleanup = false
	if err := syncDirectory(root); err != nil {
		return fmt.Errorf("sync catalog directory: %w", err)
	}
	return nil
}

func createTemporary(root *os.Root, prefix string) (string, *os.File, error) {
	for attempt := 0; attempt < 128; attempt++ {
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return "", nil, err
		}
		name := prefix + ".tmp-" + hex.EncodeToString(token[:])
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		if err := file.Chmod(fileMode); err != nil {
			_ = file.Close()
			_ = root.Remove(name)
			return "", nil, err
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != fileMode {
			_ = file.Close()
			_ = root.Remove(name)
			return "", nil, errors.New("temporary catalog file is not private and regular")
		}
		return name, file, nil
	}
	return "", nil, errors.New("create unique catalog temporary file")
}

type recordRoot interface {
	Lstat(string) (os.FileInfo, error)
	Open(string) (*os.File, error)
}

func readRecord(root recordRoot, name string, expected Identity) (Record, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return Record{}, fmt.Errorf("read catalog record %q: %w", name, err)
	}
	if !privateRegular(before) {
		return Record{}, fmt.Errorf("read catalog record %q: file must be private, regular, and not a symbolic link", name)
	}
	file, err := openRecordFile(root, name)
	if err != nil {
		return Record{}, fmt.Errorf("read catalog record %q: %w", name, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !privateRegular(opened) {
		return Record{}, fmt.Errorf("read catalog record %q: opened file must be private and regular", name)
	}
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var record Record
	if err := decoder.Decode(&record); err != nil {
		return Record{}, fmt.Errorf("read catalog record %q: %w", name, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Record{}, fmt.Errorf("read catalog record %q: trailing JSON value", name)
	}
	if record.SchemaVersion != SchemaVersion {
		return Record{}, fmt.Errorf("read catalog record %q: unsupported catalog schema version %d", name, record.SchemaVersion)
	}
	if err := validateRecord(record); err != nil {
		return Record{}, fmt.Errorf("read catalog record %q: %w", name, err)
	}
	if expected.Kind != "" && record.Kind != expected.Kind || expected.ID != "" && record.ID != expected.ID || expected.Server != "" && record.Server != expected.Server {
		return Record{}, fmt.Errorf("read catalog record %q: identity does not match path", name)
	}
	return record, nil
}

func privateRegular(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o077 == 0
}

func directoryNames(root *os.Root) ([]string, error) {
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	names, readErr := directory.Readdirnames(-1)
	closeErr := directory.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	sort.Strings(names)
	return names, nil
}

func validateQuery(query Query) error {
	if query.Kind != "" && !query.Kind.valid() {
		return errors.New("catalog kind must be markdown or html")
	}
	if query.Limit <= 0 {
		return errors.New("catalog limit must be positive")
	}
	if query.Offset < 0 {
		return errors.New("catalog offset must not be negative")
	}
	return nil
}

func validateIdentity(identity Identity) error {
	if err := validateLocation(identity.Server, identity.Kind); err != nil {
		return err
	}
	if common.ValidateID(identity.ID) != nil {
		return errors.New("invalid catalog resource id")
	}
	return nil
}

func validateLocation(server string, kind Kind) error {
	canonical, err := config.CanonicalPublishingOrigin(server)
	if err != nil || canonical != server {
		return errors.New("invalid catalog server")
	}
	if !kind.valid() {
		return errors.New("invalid catalog kind")
	}
	return nil
}

func validateRecord(record Record) error {
	if record.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported catalog schema version %d", record.SchemaVersion)
	}
	if err := validateIdentity(record.Identity()); err != nil {
		return err
	}
	if !validMetadata(record.Title) || !validMetadata(record.Summary) {
		return errors.New("catalog title and summary must be non-empty trimmed UTF-8")
	}
	if record.SourceFilename == "" || !utf8.ValidString(record.SourceFilename) || filepath.Base(record.SourceFilename) != record.SourceFilename || record.SourceFilename == "." || record.SourceFilename == ".." {
		return errors.New("invalid catalog source filename")
	}
	if record.CreatedAt <= 0 || record.UpdatedAt < record.CreatedAt {
		return errors.New("invalid catalog timestamps")
	}
	if record.Permanent != (record.ExpiresAt == nil) {
		return errors.New("invalid catalog expiration")
	}
	switch record.State {
	case StateCreated, StateCreationUncertain:
		if record.DeletedAt != nil {
			return errors.New("non-deleted catalog record has deletion time")
		}
	case StateDeleted:
		if record.DeletedAt == nil || *record.DeletedAt < record.CreatedAt || *record.DeletedAt != record.UpdatedAt {
			return errors.New("invalid catalog deletion time")
		}
	default:
		return errors.New("invalid catalog state")
	}
	parsed, err := url.Parse(record.URL)
	if err != nil || parsed.User != nil || parsed.Opaque != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return errors.New("invalid catalog capability URL")
	}
	origin, err := config.CanonicalPublishingOrigin(parsed.Scheme + "://" + parsed.Host)
	if err != nil || origin != record.Server || parsed.Path != "/whiteboards/"+string(record.Kind)+"/"+record.ID {
		return errors.New("invalid catalog capability URL")
	}
	return nil
}

func validMetadata(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && utf8.ValidString(value)
}

func (kind Kind) valid() bool { return kind == KindMarkdown || kind == KindHTML }

func chmodRootDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	opened, err := directory.Stat()
	if err != nil || !opened.IsDir() {
		return errors.New("opened catalog root is not a directory")
	}
	rootInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(opened, rootInfo) {
		return errors.New("opened catalog root identity mismatch")
	}
	return directory.Chmod(directoryMode)
}
