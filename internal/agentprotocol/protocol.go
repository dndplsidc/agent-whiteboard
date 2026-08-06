package agentprotocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
	"unicode/utf8"

	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/contextdigest"
)

const (
	APIVersion           = "1"
	Namespace            = "/api/v1/agent"
	StatusPath           = Namespace + "/status"
	ConnectPath          = Namespace + "/connect"
	WebSocketSubprotocol = "agent-whiteboard.v1"
	APIVersionHeader     = "X-Agent-Whiteboard-API-Version"

	// MaxContextCommandBytes retains ample room for the complete default context.
	// Ordinary frames allow a valid message to expand under JSON escaping while
	// MaxMessageBytes remains the logical user/assistant message limit.
	MaxContextCommandBytes = 67 << 20
	MaxMessageBytes        = 64 << 10
	// Three times the logical message limit covers worst-case 2x JSON string
	// escaping plus the complete ordinary command envelope.
	MaxOrdinaryCommandBytes = 3 * MaxMessageBytes
	MaxMarkdownBytes        = 10 << 20
	MaxCreatorContextBytes  = 1 << 20
	MaxTitleBytes           = 512
	MaxURLBytes             = 8 << 10
	MaxSummaryBytes         = 8 << 10
	MaxQueueItems           = 64
	MaxQueueBytes           = 96 << 10
	MaxTimelineBytes        = 96 << 10
	MaxEventBytes           = 256 << 10
	MaxDeltaBytes           = 32 << 10
	MaxReplayEvents         = 2048
	MaxReplayBytes          = 8 << 20
	MaxPageSize             = 100
	DefaultPageSize         = 50
)

var (
	ErrInvalidMessage  = errors.New("invalid agent protocol message")
	ErrMessageTooLarge = errors.New("agent protocol message too large")
)

type ProviderName string

const (
	ProviderPi    ProviderName = "pi"
	ProviderCodex ProviderName = "codex"
)

func (name ProviderName) Valid() bool { return name == ProviderPi || name == ProviderCodex }

func AllProviderNames() []ProviderName { return []ProviderName{ProviderPi, ProviderCodex} }

type ResourceKind string

const ResourceMarkdown ResourceKind = "markdown"

type CommandType string

const (
	CommandConnect            CommandType = "connect"
	CommandSubmit             CommandType = "submit"
	CommandQueueEdit          CommandType = "queue_edit"
	CommandQueueRemove        CommandType = "queue_remove"
	CommandInterrupt          CommandType = "interrupt"
	CommandRetry              CommandType = "retry"
	CommandNew                CommandType = "new"
	CommandArchiveList        CommandType = "archive_list"
	CommandArchiveRestore     CommandType = "archive_restore"
	CommandArchiveDelete      CommandType = "archive_delete"
	CommandHistoryPage        CommandType = "history_page"
	CommandResync             CommandType = "resync"
	CommandInteractionRespond CommandType = "interaction_respond"
)

type ContextRevision string

const (
	ContextInitial     ContextRevision = "initial"
	ContextReplacement ContextRevision = "replacement"
)

type Resource struct {
	Kind      ResourceKind `json:"kind"`
	ID        string       `json:"id"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
	ExpiresAt *time.Time   `json:"expires_at"`
}

func (resource *Resource) UnmarshalJSON(data []byte) error {
	if err := inspectJSON(data, map[string]bool{"expires_at": true}); err != nil {
		return err
	}
	type resourceWire Resource
	var wire resourceWire
	if err := strictDecode(data, &wire); err != nil {
		return err
	}
	if err := requireFields(data, "kind", "id", "created_at", "updated_at", "expires_at"); err != nil {
		return err
	}
	*resource = Resource(wire)
	return nil
}

type PageContext struct {
	Revision       ContextRevision `json:"revision"`
	Markdown       string          `json:"markdown"`
	CreatorContext string          `json:"creator_context"`
	Title          string          `json:"title"`
	URL            string          `json:"url"`
	Resource       Resource        `json:"resource"`
	Digest         string          `json:"digest"`
}

type ConnectPayload struct {
	Provider      ProviderName `json:"provider"`
	Resource      Resource     `json:"resource"`
	ContextDigest string       `json:"context_digest"`
	ReplayAfter   string       `json:"replay_after,omitempty"`
}

func (ConnectPayload) commandPayload() {}

type SubmitPayload struct {
	TurnID    string       `json:"turn_id"`
	MessageID string       `json:"message_id"`
	Message   string       `json:"message"`
	Context   *PageContext `json:"context,omitempty"`
}

func (SubmitPayload) commandPayload() {}

type QueueEditPayload struct {
	MessageID string `json:"message_id"`
	Message   string `json:"message"`
}

func (QueueEditPayload) commandPayload() {}

type MessageReferencePayload struct {
	MessageID string `json:"message_id"`
}

func (MessageReferencePayload) commandPayload() {}

type TurnReferencePayload struct {
	TurnID string `json:"turn_id"`
}

func (TurnReferencePayload) commandPayload() {}

type EmptyPayload struct{}

func (EmptyPayload) commandPayload() {}

type PageRequestPayload struct {
	Before string `json:"before,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

func (PageRequestPayload) commandPayload() {}

type ArchiveReferencePayload struct {
	ArchiveID string `json:"archive_id"`
}

func (ArchiveReferencePayload) commandPayload() {}

type ResyncPayload struct {
	AfterEventID string `json:"after_event_id,omitempty"`
}

func (ResyncPayload) commandPayload() {}

type CommandPayload interface{ commandPayload() }

type Command struct {
	APIVersion     string
	CommandID      string
	ClientID       string
	ConversationID *string
	Type           CommandType
	Payload        CommandPayload
}

type commandWire struct {
	APIVersion     string          `json:"api_version"`
	CommandID      string          `json:"command_id"`
	ClientID       string          `json:"client_id"`
	ConversationID *string         `json:"conversation_id"`
	Type           CommandType     `json:"type"`
	Payload        json.RawMessage `json:"payload"`
}

type commandMarshalWire struct {
	APIVersion     string         `json:"api_version"`
	CommandID      string         `json:"command_id"`
	ClientID       string         `json:"client_id"`
	ConversationID *string        `json:"conversation_id"`
	Type           CommandType    `json:"type"`
	Payload        CommandPayload `json:"payload"`
}

func (c Command) MarshalJSON() ([]byte, error) { return EncodeCommand(c) }

// EncodeCommand validates and encodes a command as application JSON without
// applying HTML-specific escaping.
func EncodeCommand(c Command) ([]byte, error) {
	if err := validateCommand(c); err != nil {
		return nil, err
	}
	encoded, err := marshalApplicationJSON(commandMarshalWire{c.APIVersion, c.CommandID, c.ClientID, c.ConversationID, c.Type, c.Payload})
	if err != nil {
		return nil, invalid(err)
	}
	limit := MaxOrdinaryCommandBytes
	if payload, ok := c.Payload.(SubmitPayload); ok && payload.Context != nil {
		limit = MaxContextCommandBytes
	}
	if len(encoded) > limit {
		return nil, ErrMessageTooLarge
	}
	return encoded, nil
}

func DecodeCommand(data []byte) (Command, error) {
	if len(data) > MaxContextCommandBytes {
		return Command{}, ErrMessageTooLarge
	}
	nullable := map[string]bool{
		"conversation_id":                     true,
		"payload.resource.expires_at":         true,
		"payload.context.resource.expires_at": true,
		"payload.answers":                     true,
	}
	if err := inspectJSON(data, nullable); err != nil {
		return Command{}, err
	}
	var wire commandWire
	if err := strictDecode(data, &wire); err != nil {
		return Command{}, invalid(err)
	}
	if err := requireFields(data, "api_version", "command_id", "client_id", "conversation_id", "type", "payload"); err != nil {
		return Command{}, invalid(err)
	}
	payload, err := decodeCommandPayload(wire.Type, wire.Payload)
	if err != nil {
		return Command{}, err
	}
	if len(data) > MaxOrdinaryCommandBytes {
		submit, ok := payload.(SubmitPayload)
		if !ok || submit.Context == nil {
			return Command{}, ErrMessageTooLarge
		}
	}
	command := Command{wire.APIVersion, wire.CommandID, wire.ClientID, wire.ConversationID, wire.Type, payload}
	if err := validateCommand(command); err != nil {
		return Command{}, err
	}
	return command, nil
}

func decodeCommandPayload(kind CommandType, raw json.RawMessage) (CommandPayload, error) {
	var target CommandPayload
	var required []string
	switch kind {
	case CommandConnect:
		target, required = &ConnectPayload{}, []string{"provider", "resource", "context_digest"}
	case CommandSubmit:
		target, required = &SubmitPayload{}, []string{"turn_id", "message_id", "message"}
	case CommandQueueEdit:
		target, required = &QueueEditPayload{}, []string{"message_id", "message"}
	case CommandQueueRemove:
		target, required = &MessageReferencePayload{}, []string{"message_id"}
	case CommandInterrupt, CommandRetry:
		target, required = &TurnReferencePayload{}, []string{"turn_id"}
	case CommandNew:
		target = &EmptyPayload{}
	case CommandArchiveList, CommandHistoryPage:
		target = &PageRequestPayload{}
	case CommandArchiveRestore, CommandArchiveDelete:
		target, required = &ArchiveReferencePayload{}, []string{"archive_id"}
	case CommandResync:
		target = &ResyncPayload{}
	case CommandInteractionRespond:
		target, required = &InteractionResponsePayload{}, []string{"request_id", "kind", "option_id", "answers"}
	default:
		return nil, invalid(nil)
	}
	if err := strictDecode(raw, target); err != nil {
		return nil, invalid(err)
	}
	if err := requireFields(raw, required...); err != nil {
		return nil, invalid(err)
	}
	switch value := target.(type) {
	case *ConnectPayload:
		return *value, nil
	case *SubmitPayload:
		return *value, nil
	case *QueueEditPayload:
		return *value, nil
	case *MessageReferencePayload:
		return *value, nil
	case *TurnReferencePayload:
		return *value, nil
	case *EmptyPayload:
		return *value, nil
	case *PageRequestPayload:
		return *value, nil
	case *ArchiveReferencePayload:
		return *value, nil
	case *ResyncPayload:
		return *value, nil
	case *InteractionResponsePayload:
		return *value, nil
	default:
		panic("unreachable command payload")
	}
}

func validateCommand(command Command) error {
	if command.APIVersion != APIVersion || !validID(command.CommandID) || !validID(command.ClientID) || command.Payload == nil {
		return invalid(nil)
	}
	if command.Type == CommandConnect {
		if command.ConversationID != nil {
			return invalid(nil)
		}
	} else if command.ConversationID == nil || !validID(*command.ConversationID) {
		return invalid(nil)
	}
	switch payload := command.Payload.(type) {
	case ConnectPayload:
		if command.Type != CommandConnect || !payload.Provider.Valid() || validateResource(payload.Resource) != nil || !validDigest(payload.ContextDigest) || (payload.ReplayAfter != "" && !validID(payload.ReplayAfter)) {
			return invalid(nil)
		}
	case SubmitPayload:
		if command.Type != CommandSubmit || !validID(payload.TurnID) || !validID(payload.MessageID) || !validMessage(payload.Message) {
			return invalid(nil)
		}
		if payload.Context != nil && validatePageContext(*payload.Context) != nil {
			return invalid(nil)
		}
	case QueueEditPayload:
		if command.Type != CommandQueueEdit || !validID(payload.MessageID) || !validMessage(payload.Message) {
			return invalid(nil)
		}
	case MessageReferencePayload:
		if command.Type != CommandQueueRemove || !validID(payload.MessageID) {
			return invalid(nil)
		}
	case TurnReferencePayload:
		if (command.Type != CommandInterrupt && command.Type != CommandRetry) || !validID(payload.TurnID) {
			return invalid(nil)
		}
	case EmptyPayload:
		if command.Type != CommandNew {
			return invalid(nil)
		}
	case PageRequestPayload:
		if command.Type != CommandArchiveList && command.Type != CommandHistoryPage {
			return invalid(nil)
		}
		if payload.Before != "" && !validID(payload.Before) {
			return invalid(nil)
		}
		if payload.Limit < 0 || payload.Limit > MaxPageSize {
			return invalid(nil)
		}
	case ArchiveReferencePayload:
		if (command.Type != CommandArchiveRestore && command.Type != CommandArchiveDelete) || !validID(payload.ArchiveID) {
			return invalid(nil)
		}
	case ResyncPayload:
		if command.Type != CommandResync || (payload.AfterEventID != "" && !validID(payload.AfterEventID)) {
			return invalid(nil)
		}
	case InteractionResponsePayload:
		if command.Type != CommandInteractionRespond || payload.validate() != nil {
			return invalid(nil)
		}
	default:
		return invalid(nil)
	}
	return nil
}

func validatePageContext(context PageContext) error {
	if context.Revision != ContextInitial && context.Revision != ContextReplacement {
		return invalid(nil)
	}
	if !validBoundedUTF8(context.Markdown, MaxMarkdownBytes, true) || !validBoundedUTF8(context.CreatorContext, MaxCreatorContextBytes, true) {
		return invalid(nil)
	}
	if !validBoundedText(context.Title, MaxTitleBytes, true) || !validPageURL(context.URL) || validateResource(context.Resource) != nil || context.Digest != contextdigest.Calculate([]byte(context.Markdown), []byte(context.CreatorContext)) {
		return invalid(nil)
	}
	return nil
}

func validateResource(resource Resource) error {
	if resource.Kind != ResourceMarkdown || !validID(resource.ID) || resource.CreatedAt.IsZero() || resource.UpdatedAt.IsZero() || resource.UpdatedAt.Before(resource.CreatedAt) {
		return invalid(nil)
	}
	if resource.ExpiresAt != nil && resource.ExpiresAt.Before(resource.CreatedAt) {
		return invalid(nil)
	}
	return nil
}

func validPageURL(value string) bool {
	if !validBoundedText(value, MaxURLBytes, true) {
		return false
	}
	return common.ValidPageURL(value)
}
func validMessage(value string) bool { return validBoundedText(value, MaxMessageBytes, true) }
func validBoundedText(value string, max int, nonempty bool) bool {
	return validBoundedUTF8(value, max, nonempty) && !hasDisallowedC0(value)
}
func validBoundedUTF8(value string, max int, nonempty bool) bool {
	return utf8.ValidString(value) && len(value) <= max && (!nonempty || value != "")
}
func hasDisallowedC0(value string) bool {
	for _, char := range value {
		if char < 0x20 && char != '\t' && char != '\n' && char != '\r' {
			return true
		}
	}
	return false
}
func validID(value string) bool { return common.ValidateID(value) == nil }
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

func NormalizePageSize(size int) int {
	if size == 0 {
		return DefaultPageSize
	}
	if size < 0 || size > MaxPageSize {
		return 0
	}
	return size
}

func invalid(cause error) error {
	if cause == nil {
		return ErrInvalidMessage
	}
	return fmt.Errorf("%w: %v", ErrInvalidMessage, cause)
}

func marshalApplicationJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	encoded := buffer.Bytes()
	return encoded[:len(encoded)-1], nil
}

func strictDecode(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func requireFields(data []byte, required ...string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	for _, field := range required {
		if _, exists := object[field]; !exists {
			return fmt.Errorf("missing field %q", field)
		}
	}
	return nil
}

func inspectJSON(data []byte, nullable map[string]bool) error {
	if !utf8.Valid(data) {
		return invalid(nil)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := inspectValue(decoder, "", nullable); err != nil {
		return invalid(err)
	}
	if decoder.More() {
		return invalid(nil)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return invalid(errors.New("trailing JSON value"))
	}
	return nil
}

func inspectValue(decoder *json.Decoder, path string, nullable map[string]bool) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		if !nullable[path] {
			return errors.New("unexpected null")
		}
		return nil
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			if keyErr != nil {
				return keyErr
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("invalid object key")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate field %q", key)
			}
			seen[key] = struct{}{}
			child := key
			if path != "" {
				child = path + "." + key
			}
			if err := inspectValue(decoder, child, nullable); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("invalid object")
		}
	case '[':
		for decoder.More() {
			if err := inspectValue(decoder, path+"[]", nullable); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("invalid array")
		}
	default:
		return errors.New("invalid delimiter")
	}
	return nil
}

func SplitDelta(text string) ([]string, error) {
	if !utf8.ValidString(text) {
		return nil, ErrInvalidMessage
	}
	if text == "" {
		return []string{}, nil
	}
	data := []byte(text)
	result := make([]string, 0, (len(data)+MaxDeltaBytes-1)/MaxDeltaBytes)
	for len(data) > 0 {
		end := min(len(data), MaxDeltaBytes)
		for end > 0 && !utf8.Valid(data[:end]) {
			end--
		}
		if end == 0 {
			return nil, ErrInvalidMessage
		}
		result = append(result, string(data[:end]))
		data = data[end:]
	}
	return result, nil
}
