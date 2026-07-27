package agentstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/provider"
)

const maxMappingBytes = 1 << 20

type fileIdentity struct {
	device uint64
	inode  uint64
}

type fileOps struct {
	publish                  func(string, string, *fileIdentity, fileIdentity) error
	removeExpected           func(string, fileIdentity) error
	syncDir                  func() error
	beforePathReturn         func()
	beforeWorkspaceTombstone func()
}

func defaultFileOps(directory *secureDirectory) fileOps {
	return fileOps{
		publish:                  directory.publish,
		removeExpected:           directory.removeExpected,
		syncDir:                  directory.sync,
		beforePathReturn:         func() {},
		beforeWorkspaceTombstone: func() {},
	}
}

type durableIdentity struct {
	Origin       string        `json:"origin"`
	Kind         ResourceKind  `json:"kind"`
	CapabilityID string        `json:"capability_id"`
	Provider     provider.Name `json:"provider"`
}

type durableRevision struct {
	Digest          string       `json:"digest"`
	Revision        RevisionKind `json:"revision"`
	SourceUpdatedAt time.Time    `json:"source_updated_at"`
}

type durablePrepared struct {
	Digest          string       `json:"digest"`
	Revision        RevisionKind `json:"revision"`
	SourceUpdatedAt time.Time    `json:"source_updated_at"`
	TurnID          string       `json:"turn_id"`
	Phase           CommitPhase  `json:"phase"`
}

type durableSession struct {
	ConversationID  string           `json:"conversation_id"`
	NativeReference string           `json:"native_session_ref"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
	ProviderLabel   string           `json:"provider_label"`
	ModelLabel      string           `json:"model_label"`
	Committed       *durableRevision `json:"committed"`
	Observed        *durableRevision `json:"observed"`
	PreparedCommit  *durablePrepared `json:"prepared_commit"`
}

type durableMapping struct {
	SchemaVersion int              `json:"schema_version"`
	Identity      durableIdentity  `json:"identity"`
	Current       *durableSession  `json:"current"`
	Archives      []durableSession `json:"archives"`
	CreatedAt     time.Time        `json:"created_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

func encodeMapping(mapping Mapping) ([]byte, error) {
	if err := mapping.validate(nil); err != nil {
		return nil, err
	}
	durable := durableMapping{
		SchemaVersion: mapping.SchemaVersion,
		Identity:      durableIdentity{Origin: mapping.Identity.Origin, Kind: mapping.Identity.Kind, CapabilityID: mapping.Identity.CapabilityID, Provider: mapping.Identity.Provider},
		Current:       encodeSessionPtr(mapping.Current), Archives: make([]durableSession, len(mapping.Archives)),
		CreatedAt: mapping.CreatedAt, UpdatedAt: mapping.UpdatedAt,
	}
	for index := range mapping.Archives {
		durable.Archives[index] = encodeSession(mapping.Archives[index])
	}
	return json.Marshal(durable)
}

func decodeMapping(encoded []byte, expected Identity) (Mapping, error) {
	if err := rejectDuplicateJSONFields(encoded); err != nil {
		return Mapping{}, fmt.Errorf("decode conversation mapping: %w", err)
	}
	if err := validateDurableShape(encoded); err != nil {
		return Mapping{}, fmt.Errorf("decode conversation mapping: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var durable durableMapping
	if err := decoder.Decode(&durable); err != nil {
		return Mapping{}, fmt.Errorf("decode conversation mapping: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Mapping{}, errors.New("decode conversation mapping: multiple JSON values")
		}
		return Mapping{}, fmt.Errorf("decode conversation mapping: %w", err)
	}
	mapping := Mapping{
		SchemaVersion: durable.SchemaVersion,
		Identity:      Identity{Origin: durable.Identity.Origin, Kind: durable.Identity.Kind, CapabilityID: durable.Identity.CapabilityID, Provider: durable.Identity.Provider},
		CreatedAt:     durable.CreatedAt, UpdatedAt: durable.UpdatedAt,
		Archives: make([]Session, len(durable.Archives)),
	}
	var err error
	if durable.Current != nil {
		current, decodeErr := decodeSession(*durable.Current)
		if decodeErr != nil {
			return Mapping{}, decodeErr
		}
		mapping.Current = &current
	}
	for index := range durable.Archives {
		mapping.Archives[index], err = decodeSession(durable.Archives[index])
		if err != nil {
			return Mapping{}, err
		}
	}
	if err := mapping.validate(&expected); err != nil {
		return Mapping{}, fmt.Errorf("validate conversation mapping: %w", err)
	}
	return mapping, nil
}

func encodeSessionPtr(session *Session) *durableSession {
	if session == nil {
		return nil
	}
	value := encodeSession(*session)
	return &value
}

func encodeSession(session Session) durableSession {
	return durableSession{
		ConversationID: session.ConversationID, NativeReference: session.NativeSession.Value(), CreatedAt: session.CreatedAt, UpdatedAt: session.UpdatedAt,
		ProviderLabel: session.ProviderLabel, ModelLabel: session.ModelLabel,
		Committed: encodeRevision(session.Committed), Observed: encodeRevision(session.Observed), PreparedCommit: encodePrepared(session.PreparedCommit),
	}
}

func decodeSession(durable durableSession) (Session, error) {
	ref, err := validateNativeSessionRef(durable.NativeReference)
	if err != nil {
		return Session{}, err
	}
	return Session{
		ConversationID: durable.ConversationID, NativeSession: ref, CreatedAt: durable.CreatedAt, UpdatedAt: durable.UpdatedAt,
		ProviderLabel: durable.ProviderLabel, ModelLabel: durable.ModelLabel,
		Committed: decodeRevision(durable.Committed), Observed: decodeRevision(durable.Observed), PreparedCommit: decodePrepared(durable.PreparedCommit),
	}, nil
}

func encodeRevision(revision *Revision) *durableRevision {
	if revision == nil {
		return nil
	}
	return &durableRevision{Digest: revision.Digest, Revision: revision.Revision, SourceUpdatedAt: revision.SourceUpdatedAt}
}

func decodeRevision(revision *durableRevision) *Revision {
	if revision == nil {
		return nil
	}
	return &Revision{Digest: revision.Digest, Revision: revision.Revision, SourceUpdatedAt: revision.SourceUpdatedAt}
}

func encodePrepared(prepared *PreparedCommit) *durablePrepared {
	if prepared == nil {
		return nil
	}
	return &durablePrepared{Digest: prepared.Digest, Revision: prepared.Revision.Revision, SourceUpdatedAt: prepared.SourceUpdatedAt, TurnID: prepared.TurnID, Phase: prepared.Phase}
}

func decodePrepared(prepared *durablePrepared) *PreparedCommit {
	if prepared == nil {
		return nil
	}
	return &PreparedCommit{Revision: Revision{Digest: prepared.Digest, Revision: prepared.Revision, SourceUpdatedAt: prepared.SourceUpdatedAt}, TurnID: prepared.TurnID, Phase: prepared.Phase}
}

func validateDurableShape(encoded []byte) error {
	mapping, err := exactJSONObject(encoded, "schema_version", "identity", "current", "archives", "created_at", "updated_at")
	if err != nil {
		return err
	}
	for _, name := range []string{"schema_version", "created_at", "updated_at"} {
		if isJSONNull(mapping[name]) {
			return fmt.Errorf("field %q must not be null", name)
		}
	}
	identity, err := exactJSONObject(mapping["identity"], "origin", "kind", "capability_id", "provider")
	if err != nil {
		return fmt.Errorf("field %q: %w", "identity", err)
	}
	for name, value := range identity {
		if isJSONNull(value) {
			return fmt.Errorf("identity field %q must not be null", name)
		}
	}
	if !isJSONNull(mapping["current"]) {
		if err := validateDurableSessionShape(mapping["current"]); err != nil {
			return fmt.Errorf("field %q: %w", "current", err)
		}
	}
	if isJSONNull(mapping["archives"]) {
		return errors.New("field \"archives\" must be an array")
	}
	var archives []json.RawMessage
	if err := json.Unmarshal(mapping["archives"], &archives); err != nil || archives == nil {
		return errors.New("field \"archives\" must be an array")
	}
	for index, archive := range archives {
		if err := validateDurableSessionShape(archive); err != nil {
			return fmt.Errorf("archive %d: %w", index, err)
		}
	}
	return nil
}

func validateDurableSessionShape(encoded []byte) error {
	session, err := exactJSONObject(encoded, "conversation_id", "native_session_ref", "created_at", "updated_at", "provider_label", "model_label", "committed", "observed", "prepared_commit")
	if err != nil {
		return err
	}
	for _, name := range []string{"conversation_id", "native_session_ref", "created_at", "updated_at", "provider_label", "model_label"} {
		if isJSONNull(session[name]) {
			return fmt.Errorf("field %q must not be null", name)
		}
	}
	for _, name := range []string{"committed", "observed"} {
		if isJSONNull(session[name]) {
			continue
		}
		revision, err := exactJSONObject(session[name], "digest", "revision", "source_updated_at")
		if err != nil {
			return fmt.Errorf("field %q: %w", name, err)
		}
		for field, value := range revision {
			if isJSONNull(value) {
				return fmt.Errorf("field %q.%s must not be null", name, field)
			}
		}
	}
	if !isJSONNull(session["prepared_commit"]) {
		prepared, err := exactJSONObject(session["prepared_commit"], "digest", "revision", "source_updated_at", "turn_id", "phase")
		if err != nil {
			return fmt.Errorf("field %q: %w", "prepared_commit", err)
		}
		for field, value := range prepared {
			if isJSONNull(value) {
				return fmt.Errorf("field %q.%s must not be null", "prepared_commit", field)
			}
		}
	}
	return nil
}

func exactJSONObject(encoded []byte, names ...string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil || object == nil {
		return nil, errors.New("value must be an object")
	}
	if len(object) != len(names) {
		return nil, errors.New("object has missing or unexpected fields")
	}
	for _, name := range names {
		if _, exists := object[name]; !exists {
			return nil, fmt.Errorf("required field %q is missing", name)
		}
	}
	return object, nil
}

func isJSONNull(encoded []byte) bool { return bytes.Equal(bytes.TrimSpace(encoded), []byte("null")) }

func rejectDuplicateJSONFields(encoded []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if err := consumeJSONValue(decoder, token); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder, token json.Token) error {
	delimiter, structured := token.(json.Delim)
	if !structured {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("invalid JSON object key")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			value, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := consumeJSONValue(decoder, value); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
	case '[':
		for decoder.More() {
			value, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := consumeJSONValue(decoder, value); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	return nil
}

func cleanTemporaryMappings(directory *secureDirectory) error {
	names, err := directory.names()
	if err != nil {
		return err
	}
	for _, name := range names {
		if !strings.HasPrefix(name, ".mapping.tmp-") {
			continue
		}
		if _, _, err := directory.readVerified(name, maxMappingBytes); err != nil {
			return fmt.Errorf("unsafe temporary mapping %q: %w", name, err)
		}
		if err := directory.remove(name); err != nil {
			return err
		}
	}
	return directory.sync()
}

func inspectExpected(directory *secureDirectory, name string, expected []byte) CommitOutcome {
	actual, _, err := directory.readVerified(name, maxMappingBytes)
	if err == nil {
		if bytes.Equal(actual, expected) {
			return CommitApplied
		}
		return CommitNotApplied
	}
	if errors.Is(err, os.ErrNotExist) {
		return CommitNotApplied
	}
	return CommitUncertain
}
