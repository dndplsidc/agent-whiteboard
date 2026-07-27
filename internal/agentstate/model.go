// Package agentstate persists only provider-neutral conversation bookkeeping.
// Page content, messages, transcripts, previews, and provider payloads are not
// representable by its durable schema.
package agentstate

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/config"
	"github.com/edocsss/agent-whiteboard/internal/provider"
)

const (
	SchemaVersion           = 1
	MaxNativeReferenceBytes = 1024
	maxDisplayLabelBytes    = 512
)

type ResourceKind string

const ResourceMarkdown ResourceKind = "markdown"

type Identity struct {
	Origin       string
	Kind         ResourceKind
	CapabilityID string
	Provider     provider.Name
}

func (identity Identity) Validate() error {
	canonical, err := config.CanonicalOrigin(identity.Origin)
	if err != nil || canonical != identity.Origin || identity.Kind != ResourceMarkdown || common.ValidateID(identity.CapabilityID) != nil || identity.Provider != provider.NamePi {
		return errors.New("invalid conversation identity")
	}
	return nil
}

type RevisionKind string

const (
	RevisionInitial     RevisionKind = "initial"
	RevisionReplacement RevisionKind = "replacement"
)

type Revision struct {
	Digest          string
	Revision        RevisionKind
	SourceUpdatedAt time.Time
}

func (revision Revision) validate() error {
	if !validDigest(revision.Digest) || (revision.Revision != RevisionInitial && revision.Revision != RevisionReplacement) || !validStoredTime(revision.SourceUpdatedAt) {
		return errors.New("invalid context revision bookkeeping")
	}
	return nil
}

type CommitPhase string

const (
	CommitPrepared CommitPhase = "prepared"
	CommitAccepted CommitPhase = "accepted"
)

type PreparedCommit struct {
	Revision
	TurnID string
	Phase  CommitPhase
}

func (prepared PreparedCommit) validate() error {
	if prepared.Revision.validate() != nil || common.ValidateID(prepared.TurnID) != nil || (prepared.Phase != CommitPrepared && prepared.Phase != CommitAccepted) {
		return errors.New("invalid prepared commit bookkeeping")
	}
	return nil
}

type Session struct {
	ConversationID string
	NativeSession  provider.NativeSessionRef
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ProviderLabel  string
	ModelLabel     string
	Committed      *Revision
	Observed       *Revision
	PreparedCommit *PreparedCommit
}

func (session Session) validate() error {
	if common.ValidateID(session.ConversationID) != nil || !validStoredTime(session.CreatedAt) || !validStoredTime(session.UpdatedAt) || session.UpdatedAt.Before(session.CreatedAt) || !validDisplayLabel(session.ProviderLabel) || !validDisplayLabel(session.ModelLabel) {
		return errors.New("invalid session bookkeeping")
	}
	if _, err := validateNativeSessionRef(session.NativeSession.Value()); err != nil || !session.NativeSession.Valid() {
		return errors.New("invalid session bookkeeping")
	}
	if session.Committed != nil && session.Committed.validate() != nil {
		return errors.New("invalid committed revision")
	}
	if session.Observed != nil && session.Observed.validate() != nil {
		return errors.New("invalid observed revision")
	}
	if session.PreparedCommit != nil && session.PreparedCommit.validate() != nil {
		return errors.New("invalid prepared commit")
	}
	if session.PreparedCommit != nil && (session.Observed == nil || session.PreparedCommit.Revision != *session.Observed) {
		return errors.New("prepared commit does not match observed revision")
	}
	return nil
}

type Mapping struct {
	SchemaVersion int
	Identity      Identity
	Current       *Session
	Archives      []Session
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

func (mapping Mapping) validate(expected *Identity) error {
	if mapping.SchemaVersion != SchemaVersion || mapping.Identity.Validate() != nil || !validStoredTime(mapping.CreatedAt) || !validStoredTime(mapping.UpdatedAt) || mapping.UpdatedAt.Before(mapping.CreatedAt) || (mapping.Current == nil && len(mapping.Archives) == 0) {
		return errors.New("invalid conversation mapping")
	}
	if expected != nil && mapping.Identity != *expected {
		return errors.New("conversation mapping identity mismatch")
	}
	seenIDs := make(map[string]struct{}, len(mapping.Archives)+1)
	seenNative := make(map[string]struct{}, len(mapping.Archives)+1)
	validateUnique := func(session Session) error {
		if err := session.validate(); err != nil {
			return err
		}
		if _, duplicate := seenIDs[session.ConversationID]; duplicate {
			return errors.New("duplicate broker conversation ID")
		}
		if _, duplicate := seenNative[session.NativeSession.Value()]; duplicate {
			return errors.New("duplicate native session reference")
		}
		seenIDs[session.ConversationID] = struct{}{}
		seenNative[session.NativeSession.Value()] = struct{}{}
		return nil
	}
	if mapping.Current != nil {
		if err := validateUnique(*mapping.Current); err != nil {
			return err
		}
	}
	for index := range mapping.Archives {
		if err := validateUnique(mapping.Archives[index]); err != nil {
			return err
		}
	}
	return nil
}

func NativeSessionRef(value string) (provider.NativeSessionRef, error) {
	return validateNativeSessionRef(value)
}

func validateNativeSessionRef(value string) (provider.NativeSessionRef, error) {
	if value == "" || len(value) > MaxNativeReferenceBytes || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') || filepath.IsAbs(value) || filepath.Clean(value) != value || value == "." {
		return provider.NativeSessionRef{}, errors.New("invalid native session reference")
	}
	for _, component := range strings.Split(filepath.ToSlash(value), "/") {
		if component == "" || component == "." || component == ".." {
			return provider.NativeSessionRef{}, errors.New("invalid native session reference")
		}
	}
	ref, err := provider.NewNativeSessionRef(value)
	if err != nil {
		return provider.NativeSessionRef{}, fmt.Errorf("invalid native session reference: %w", err)
	}
	return ref, nil
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validStoredTime(value time.Time) bool {
	if value.IsZero() {
		return false
	}
	_, offset := value.Zone()
	return offset == 0 && value.Location() == time.UTC
}

func validDisplayLabel(value string) bool {
	if !utf8.ValidString(value) || len(value) > maxDisplayLabelBytes {
		return false
	}
	for _, char := range value {
		if char < 0x20 && char != '\t' && char != '\n' && char != '\r' {
			return false
		}
	}
	return true
}
