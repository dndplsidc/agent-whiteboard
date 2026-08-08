// Package agentattachment owns private, conversation-local image attachments
// used by the loopback Page Agent broker.
package agentattachment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/edocsss/agent-whiteboard/internal/agentlimits"
	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/provider"
	"github.com/edocsss/agent-whiteboard/internal/raster"
)

const (
	attachmentDirectory = ".agent-images"
	manifestName        = "manifest.json"
	manifestSchema      = 1
	maxManifestBytes    = 4 << 20
	stagedLifetime      = 15 * time.Minute
)

var (
	ErrInvalid        = errors.New("invalid image attachment request")
	ErrUnsupported    = errors.New("unsupported image attachment")
	ErrImageTooLarge  = errors.New("image attachment is too large")
	ErrTurnLimit      = errors.New("image attachment turn limit exceeded")
	ErrWorkspaceLimit = errors.New("image attachment workspace limit exceeded")
	ErrMissing        = errors.New("image attachment is unavailable")
	ErrStorage        = errors.New("image attachment storage failure")
)

type WorkspaceProvider interface {
	EnsureWorkspace(conversationID string) (string, error)
	RemoveWorkspace(conversationID string) error
}

type Service struct {
	mu         sync.Mutex
	workspaces WorkspaceProvider
	clock      common.Clock
	ids        common.IDGenerator
	removed    map[string]struct{}
}

func New(workspaces WorkspaceProvider, clock common.Clock, ids common.IDGenerator) (*Service, error) {
	if common.IsNil(workspaces) || common.IsNil(clock) || common.IsNil(ids) {
		return nil, ErrInvalid
	}
	return &Service{workspaces: workspaces, clock: clock, ids: ids, removed: make(map[string]struct{})}, nil
}

type StageRequest struct {
	Origin         string
	Provider       provider.Name
	ConversationID string
	ClientID       string
	Content        io.Reader
}

type Staged struct {
	ImageID   string `json:"image_id"`
	MediaType string `json:"media_type"`
	Bytes     int64  `json:"bytes"`
}

type ReadRequest struct {
	Origin         string
	ConversationID string
	ClientID       string
	ImageID        string
}

type Image struct {
	MediaType string
	Content   []byte
}

type DeleteRequest = ReadRequest

type ClaimRequest struct {
	Origin         string
	Provider       provider.Name
	ConversationID string
	ClientID       string
	TurnID         string
	MessageID      string
	Images         []agentprotocol.ImageReference
}

type Claimed struct {
	Inputs      []provider.ImageInput
	Descriptors []agentprotocol.ImageDescriptor
}

type lifecycle string

const (
	stagedLifecycle  lifecycle = "staged"
	claimedLifecycle lifecycle = "claimed"
)

type record struct {
	ImageID        string        `json:"image_id"`
	Filename       string        `json:"filename"`
	MediaType      string        `json:"media_type"`
	Bytes          int64         `json:"bytes"`
	Origin         string        `json:"origin"`
	Provider       provider.Name `json:"provider"`
	ConversationID string        `json:"conversation_id"`
	ClientID       string        `json:"client_id"`
	State          lifecycle     `json:"state"`
	Name           string        `json:"name,omitempty"`
	TurnID         string        `json:"turn_id,omitempty"`
	MessageID      string        `json:"message_id,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
	ExpiresAt      time.Time     `json:"expires_at,omitempty"`
}

type manifest struct {
	Schema  int      `json:"schema"`
	Records []record `json:"records"`
}

func (service *Service) Stage(ctx context.Context, request StageRequest) (Staged, error) {
	if service == nil || !validOrigin(request.Origin) || !request.Provider.Valid() || common.ValidateID(request.ConversationID) != nil || common.ValidateID(request.ClientID) != nil || common.IsNil(request.Content) {
		return Staged{}, ErrInvalid
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Staged{}, err
	}

	root, _, err := service.open(request.ConversationID)
	if err != nil {
		return Staged{}, ErrStorage
	}
	defer root.Close()
	state, err := loadManifest(root, request.ConversationID)
	if err != nil {
		return Staged{}, ErrStorage
	}
	now := service.clock.Now().UTC()
	if expired := removeExpiredRecords(&state, now); len(expired) != 0 {
		if saveManifest(root, state, service.ids) != nil || removeFiles(root, expired) != nil {
			return Staged{}, ErrStorage
		}
	}

	var draftCount int
	var draftBytes, conversationBytes int64
	for _, existing := range state.Records {
		conversationBytes += existing.Bytes
		if existing.State == stagedLifecycle && existing.ClientID == request.ClientID {
			draftCount++
			draftBytes += existing.Bytes
		}
	}
	if draftCount >= agentlimits.MaxImagesPerTurn {
		return Staged{}, ErrTurnLimit
	}

	id, err := service.ids.NewID()
	if err != nil || common.ValidateID(id) != nil {
		return Staged{}, ErrStorage
	}
	temporary := id + ".uploading"
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Staged{}, ErrStorage
	}
	var content bytes.Buffer
	written, copyErr := io.Copy(io.MultiWriter(file, &content), io.LimitReader(request.Content, int64(agentlimits.MaxImageBytes)+1))
	syncErr := file.Sync()
	closeErr := file.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = root.Remove(temporary)
		return Staged{}, ErrStorage
	}
	if written > int64(agentlimits.MaxImageBytes) {
		_ = root.Remove(temporary)
		return Staged{}, ErrImageTooLarge
	}
	format, err := raster.Detect(content.Bytes())
	if err != nil {
		_ = root.Remove(temporary)
		return Staged{}, ErrUnsupported
	}
	if draftBytes+written > int64(agentlimits.MaxTurnImageBytes) {
		_ = root.Remove(temporary)
		return Staged{}, ErrTurnLimit
	}
	if conversationBytes+written > agentlimits.MaxConversationImageBytes {
		_ = root.Remove(temporary)
		return Staged{}, ErrWorkspaceLimit
	}
	filename := id + format.Extension
	if err := root.Rename(temporary, filename); err != nil || syncRoot(root) != nil {
		_ = root.Remove(temporary)
		_ = root.Remove(filename)
		return Staged{}, ErrStorage
	}
	state.Records = append(state.Records, record{
		ImageID: id, Filename: filename, MediaType: format.MediaType, Bytes: written,
		Origin: request.Origin, Provider: request.Provider, ConversationID: request.ConversationID,
		ClientID: request.ClientID, State: stagedLifecycle, CreatedAt: now, ExpiresAt: now.Add(stagedLifetime),
	})
	if err := saveManifest(root, state, service.ids); err != nil {
		// Publication may have reached the manifest rename before a directory
		// sync failed. Preserve the image in either case: it is then either the
		// staged record's live file or a recognized orphan removed by Sweep.
		return Staged{}, ErrStorage
	}
	return Staged{ImageID: id, MediaType: format.MediaType, Bytes: written}, nil
}

func (service *Service) Read(ctx context.Context, request ReadRequest) (Image, error) {
	if service == nil || !validOrigin(request.Origin) || common.ValidateID(request.ConversationID) != nil || common.ValidateID(request.ClientID) != nil || common.ValidateID(request.ImageID) != nil {
		return Image{}, ErrInvalid
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Image{}, err
	}
	root, _, err := service.open(request.ConversationID)
	if err != nil {
		return Image{}, ErrMissing
	}
	defer root.Close()
	state, err := loadManifest(root, request.ConversationID)
	if err != nil {
		return Image{}, ErrStorage
	}
	index := findRecord(state.Records, request.ImageID)
	if index < 0 {
		return Image{}, ErrMissing
	}
	record := state.Records[index]
	if record.Origin != request.Origin || (record.State == stagedLifecycle && record.ClientID != request.ClientID) || (record.State == stagedLifecycle && !record.ExpiresAt.After(service.clock.Now().UTC())) {
		return Image{}, ErrMissing
	}
	content, err := readSecureFile(root, record.Filename, record.Bytes)
	if err != nil {
		return Image{}, ErrStorage
	}
	return Image{MediaType: record.MediaType, Content: content}, nil
}

func (service *Service) DeleteStaged(ctx context.Context, request DeleteRequest) error {
	if service == nil || !validOrigin(request.Origin) || common.ValidateID(request.ConversationID) != nil || common.ValidateID(request.ClientID) != nil || common.ValidateID(request.ImageID) != nil {
		return ErrInvalid
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	root, _, err := service.open(request.ConversationID)
	if err != nil {
		return ErrMissing
	}
	defer root.Close()
	state, err := loadManifest(root, request.ConversationID)
	if err != nil {
		return ErrStorage
	}
	index := findRecord(state.Records, request.ImageID)
	if index < 0 || state.Records[index].State != stagedLifecycle || state.Records[index].Origin != request.Origin || state.Records[index].ClientID != request.ClientID {
		return ErrMissing
	}
	removed := state.Records[index]
	state.Records = slices.Delete(state.Records, index, index+1)
	if err := saveManifest(root, state, service.ids); err != nil {
		return ErrStorage
	}
	if err := root.Remove(removed.Filename); err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrStorage
	}
	return syncRoot(root)
}

func (service *Service) Claim(ctx context.Context, request ClaimRequest) (Claimed, error) {
	if service == nil || !validOrigin(request.Origin) || !request.Provider.Valid() || common.ValidateID(request.ConversationID) != nil || common.ValidateID(request.ClientID) != nil || common.ValidateID(request.TurnID) != nil || common.ValidateID(request.MessageID) != nil || len(request.Images) == 0 || len(request.Images) > agentlimits.MaxImagesPerTurn {
		return Claimed{}, ErrInvalid
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Claimed{}, err
	}
	root, directory, err := service.open(request.ConversationID)
	if err != nil {
		return Claimed{}, ErrMissing
	}
	defer root.Close()
	state, err := loadManifest(root, request.ConversationID)
	if err != nil {
		return Claimed{}, ErrStorage
	}
	now := service.clock.Now().UTC()
	indices := make([]int, len(request.Images))
	inputs := make([]provider.ImageInput, len(request.Images))
	descriptors := make([]agentprotocol.ImageDescriptor, len(request.Images))
	seen := make(map[string]struct{}, len(request.Images))
	for position, reference := range request.Images {
		if _, duplicate := seen[reference.ImageID]; duplicate {
			return Claimed{}, ErrInvalid
		}
		seen[reference.ImageID] = struct{}{}
		index := findRecord(state.Records, reference.ImageID)
		if index < 0 {
			return Claimed{}, ErrMissing
		}
		record := state.Records[index]
		if record.State != stagedLifecycle || record.Origin != request.Origin || record.Provider != request.Provider || record.ClientID != request.ClientID || !record.ExpiresAt.After(now) {
			return Claimed{}, ErrMissing
		}
		input := provider.ImageInput{ID: record.ImageID, Name: reference.Name, MediaType: record.MediaType, Bytes: record.Bytes, Path: filepath.Join(directory, record.Filename)}
		if input.Validate() != nil {
			return Claimed{}, ErrInvalid
		}
		if _, err := readSecureFile(root, record.Filename, record.Bytes); err != nil {
			return Claimed{}, ErrStorage
		}
		indices[position] = index
		inputs[position] = input
		descriptors[position] = agentprotocol.ImageDescriptor{ImageID: record.ImageID, Name: reference.Name, MediaType: record.MediaType}
	}
	for position, index := range indices {
		state.Records[index].State = claimedLifecycle
		state.Records[index].Name = request.Images[position].Name
		state.Records[index].TurnID = request.TurnID
		state.Records[index].MessageID = request.MessageID
		state.Records[index].ExpiresAt = time.Time{}
	}
	if err := saveManifest(root, state, service.ids); err != nil {
		return Claimed{}, ErrStorage
	}
	return Claimed{Inputs: inputs, Descriptors: descriptors}, nil
}

func (service *Service) ImagesForMessage(ctx context.Context, conversationID, messageID string) ([]agentprotocol.ImageDescriptor, error) {
	if service == nil || common.ValidateID(conversationID) != nil || common.ValidateID(messageID) != nil {
		return nil, ErrInvalid
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, _, err := service.open(conversationID)
	if err != nil {
		return []agentprotocol.ImageDescriptor{}, nil
	}
	defer root.Close()
	state, err := loadManifest(root, conversationID)
	if err != nil {
		return nil, ErrStorage
	}
	result := make([]agentprotocol.ImageDescriptor, 0)
	for _, record := range state.Records {
		if record.State == claimedLifecycle && record.MessageID == messageID {
			result = append(result, descriptor(record))
		}
	}
	return result, nil
}

func (service *Service) ReleaseMessage(ctx context.Context, conversationID, messageID string) error {
	if service == nil || common.ValidateID(conversationID) != nil || common.ValidateID(messageID) != nil {
		return ErrInvalid
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	root, _, err := service.open(conversationID)
	if err != nil {
		return nil
	}
	defer root.Close()
	state, err := loadManifest(root, conversationID)
	if err != nil {
		return ErrStorage
	}
	kept := make([]record, 0, len(state.Records))
	removed := make([]record, 0)
	for _, record := range state.Records {
		if record.MessageID == messageID {
			removed = append(removed, record)
		} else {
			kept = append(kept, record)
		}
	}
	if len(removed) == 0 {
		return nil
	}
	state.Records = kept
	if err := saveManifest(root, state, service.ids); err != nil {
		return ErrStorage
	}
	for _, record := range removed {
		if err := root.Remove(record.Filename); err != nil && !errors.Is(err, os.ErrNotExist) {
			return ErrStorage
		}
	}
	return syncRoot(root)
}

func (service *Service) Sweep(ctx context.Context, conversationID string) error {
	if service == nil || common.ValidateID(conversationID) != nil {
		return ErrInvalid
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	root, _, err := service.open(conversationID)
	if err != nil {
		return ErrStorage
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return ErrStorage
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".uploading") || strings.HasSuffix(entry.Name(), ".manifest") {
			if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || root.Remove(entry.Name()) != nil {
				return ErrStorage
			}
		}
	}
	state, err := loadManifest(root, conversationID)
	if err != nil {
		return ErrStorage
	}
	if expired := removeExpiredRecords(&state, service.clock.Now().UTC()); len(expired) != 0 {
		if saveManifest(root, state, service.ids) != nil || removeFiles(root, expired) != nil {
			return ErrStorage
		}
	}
	wanted := map[string]struct{}{manifestName: {}}
	for _, record := range state.Records {
		wanted[record.Filename] = struct{}{}
	}
	entries, err = fs.ReadDir(root.FS(), ".")
	if err != nil {
		return ErrStorage
	}
	for _, entry := range entries {
		if _, exists := wanted[entry.Name()]; exists {
			continue
		}
		if !validImageFilename(entry.Name()) || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return ErrStorage
		}
		if err := root.Remove(entry.Name()); err != nil {
			return ErrStorage
		}
	}
	return syncRoot(root)
}

// RemoveWorkspace serializes conversation deletion with every staged-image
// operation. A request authorized just before deletion therefore cannot
// recreate the removed workspace after cleanup completes.
func (service *Service) RemoveWorkspace(ctx context.Context, conversationID string) error {
	if service == nil || common.ValidateID(conversationID) != nil {
		return ErrInvalid
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := service.workspaces.RemoveWorkspace(conversationID); err != nil {
		return ErrStorage
	}
	service.removed[conversationID] = struct{}{}
	return nil
}

func (service *Service) open(conversationID string) (*os.Root, string, error) {
	if _, removed := service.removed[conversationID]; removed {
		return nil, "", ErrStorage
	}
	workspace, err := service.workspaces.EnsureWorkspace(conversationID)
	if err != nil || !filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace {
		return nil, "", ErrStorage
	}
	before, err := os.Lstat(workspace)
	if err != nil || !secureDirectory(before) {
		return nil, "", ErrStorage
	}
	workspaceRoot, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, "", ErrStorage
	}
	after, err := workspaceRoot.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		workspaceRoot.Close()
		return nil, "", ErrStorage
	}
	entry, err := workspaceRoot.Lstat(attachmentDirectory)
	if errors.Is(err, os.ErrNotExist) {
		if err := workspaceRoot.Mkdir(attachmentDirectory, 0o700); err != nil {
			workspaceRoot.Close()
			return nil, "", ErrStorage
		}
		entry, err = workspaceRoot.Lstat(attachmentDirectory)
	}
	if err != nil || !secureDirectory(entry) {
		workspaceRoot.Close()
		return nil, "", ErrStorage
	}
	root, err := workspaceRoot.OpenRoot(attachmentDirectory)
	if err != nil {
		workspaceRoot.Close()
		return nil, "", ErrStorage
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(entry, opened) {
		root.Close()
		workspaceRoot.Close()
		return nil, "", ErrStorage
	}
	workspaceRoot.Close()
	return root, filepath.Join(workspace, attachmentDirectory), nil
}

func loadManifest(root *os.Root, conversationID string) (manifest, error) {
	info, err := root.Lstat(manifestName)
	if errors.Is(err, os.ErrNotExist) {
		return manifest{Schema: manifestSchema, Records: []record{}}, nil
	}
	if err != nil || !secureRegular(info) || info.Size() > maxManifestBytes {
		return manifest{}, ErrStorage
	}
	content, err := root.ReadFile(manifestName)
	if err != nil || int64(len(content)) != info.Size() {
		return manifest{}, ErrStorage
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var state manifest
	if decoder.Decode(&state) != nil || decoder.Decode(&struct{}{}) != io.EOF || state.Schema != manifestSchema || state.Records == nil {
		return manifest{}, ErrStorage
	}
	seen := make(map[string]struct{}, len(state.Records))
	for _, record := range state.Records {
		if !validRecord(record, conversationID) {
			return manifest{}, ErrStorage
		}
		if _, duplicate := seen[record.ImageID]; duplicate {
			return manifest{}, ErrStorage
		}
		seen[record.ImageID] = struct{}{}
		fileInfo, err := root.Lstat(record.Filename)
		if err != nil || !secureRegular(fileInfo) || fileInfo.Size() != record.Bytes {
			return manifest{}, ErrStorage
		}
	}
	return state, nil
}

func saveManifest(root *os.Root, state manifest, ids common.IDGenerator) error {
	encoded, err := json.Marshal(state)
	if err != nil || len(encoded)+1 > maxManifestBytes {
		return ErrStorage
	}
	encoded = append(encoded, '\n')
	id, err := ids.NewID()
	if err != nil || common.ValidateID(id) != nil {
		return ErrStorage
	}
	temporary := id + ".manifest"
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return ErrStorage
	}
	_, writeErr := file.Write(encoded)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		_ = root.Remove(temporary)
		return ErrStorage
	}
	if err := root.Rename(temporary, manifestName); err != nil {
		_ = root.Remove(temporary)
		return ErrStorage
	}
	return syncRoot(root)
}

func readSecureFile(root *os.Root, name string, expected int64) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil || !secureRegular(info) || info.Size() != expected || expected <= 0 || expected > int64(agentlimits.MaxImageBytes) {
		return nil, ErrStorage
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, ErrStorage
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		file.Close()
		return nil, ErrStorage
	}
	content, readErr := io.ReadAll(io.LimitReader(file, expected+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || int64(len(content)) != expected {
		return nil, ErrStorage
	}
	return content, nil
}

func removeExpiredRecords(state *manifest, now time.Time) []string {
	kept := make([]record, 0, len(state.Records))
	removed := make([]string, 0)
	for _, record := range state.Records {
		if record.State == stagedLifecycle && !record.ExpiresAt.After(now) {
			removed = append(removed, record.Filename)
			continue
		}
		kept = append(kept, record)
	}
	state.Records = kept
	return removed
}

func removeFiles(root *os.Root, names []string) error {
	for _, name := range names {
		if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return syncRoot(root)
}

func validRecord(record record, conversationID string) bool {
	if common.ValidateID(record.ImageID) != nil || record.ConversationID != conversationID || common.ValidateID(record.ClientID) != nil || !record.Provider.Valid() || !validOrigin(record.Origin) || !raster.SupportedMediaType(record.MediaType) || record.Bytes <= 0 || record.Bytes > int64(agentlimits.MaxImageBytes) || record.CreatedAt.IsZero() || record.Filename != record.ImageID+extensionFor(record.MediaType) {
		return false
	}
	switch record.State {
	case stagedLifecycle:
		return record.Name == "" && record.TurnID == "" && record.MessageID == "" && !record.ExpiresAt.IsZero()
	case claimedLifecycle:
		return validName(record.Name) && common.ValidateID(record.TurnID) == nil && common.ValidateID(record.MessageID) == nil && record.ExpiresAt.IsZero()
	default:
		return false
	}
}

func descriptor(record record) agentprotocol.ImageDescriptor {
	return agentprotocol.ImageDescriptor{ImageID: record.ImageID, Name: record.Name, MediaType: record.MediaType}
}

func findRecord(records []record, imageID string) int {
	for index := range records {
		if records[index].ImageID == imageID {
			return index
		}
	}
	return -1
}

func validOrigin(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != "" && parsed.User == nil && parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.String() == value
}

func validName(value string) bool {
	return value != "" && utf8.ValidString(value) && len(value) <= agentlimits.MaxImageNameBytes && !strings.ContainsRune(value, '\x00')
}

func validImageFilename(name string) bool {
	extension := filepath.Ext(name)
	id := strings.TrimSuffix(name, extension)
	return common.ValidateID(id) == nil && (extension == ".png" || extension == ".jpg" || extension == ".gif" || extension == ".webp")
}

func extensionFor(mediaType string) string {
	switch mediaType {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ""
	}
}

func syncRoot(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open attachment directory: %w", err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}
