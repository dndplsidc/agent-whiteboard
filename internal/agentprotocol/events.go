package agentprotocol

import (
	"encoding/json"
	"time"
	"unicode/utf8"
)

type EventType string

const (
	EventSnapshot         EventType = "snapshot"
	EventCommandResult    EventType = "command_result"
	EventTimeline         EventType = "timeline"
	EventHistory          EventType = "history"
	EventUserMessage      EventType = "user_message"
	EventAssistantDelta   EventType = "assistant_delta"
	EventAssistantMessage EventType = "assistant_message"
	EventQueue            EventType = "queue"
	EventLifecycle        EventType = "lifecycle"
	EventProvider         EventType = "provider"
	EventContext          EventType = "context"
	EventActivity         EventType = "activity"
	EventBlocked          EventType = "blocked"
	EventError            EventType = "error"
	EventCompletion       EventType = "completion"
	EventInterruption     EventType = "interruption"
	EventArchive          EventType = "archive"
)

type LifecycleState string

const (
	LifecycleConnecting  LifecycleState = "connecting"
	LifecycleReady       LifecycleState = "ready"
	LifecycleResponding  LifecycleState = "responding"
	LifecycleInterrupted LifecycleState = "interrupted"
	LifecycleUnavailable LifecycleState = "unavailable"
)

type ContextState string

const (
	ContextPending     ContextState = "pending"
	ContextAccepted    ContextState = "accepted"
	ContextUnchanged   ContextState = "unchanged"
	ContextUnavailable ContextState = "unavailable"
)

type CommandStatus string

const (
	CommandSucceeded CommandStatus = "succeeded"
	CommandRejected  CommandStatus = "rejected"
)

type ProviderState string

const (
	ProviderStarting    ProviderState = "starting"
	ProviderReady       ProviderState = "ready"
	ProviderUnavailable ProviderState = "unavailable"
	ProviderRecovering  ProviderState = "recovering"
)

type ActivityKind string

const (
	ActivityStatus         ActivityKind = "status"
	ActivityVisibleSummary ActivityKind = "visible_summary"
	ActivityRetry          ActivityKind = "retry"
	ActivityCompaction     ActivityKind = "compaction"
)

type BlockedKind string

const (
	BlockedTool       BlockedKind = "tool"
	BlockedPermission BlockedKind = "permission"
)

type InterruptionReason string

const (
	InterruptionRequested    InterruptionReason = "requested"
	InterruptionProviderExit InterruptionReason = "provider_exit"
	InterruptionShutdown     InterruptionReason = "shutdown"
)

type ArchiveAction string

const (
	ArchiveCreated  ArchiveAction = "created"
	ArchiveRestored ArchiveAction = "restored"
	ArchiveDeleted  ArchiveAction = "deleted"
)

type TimelineItemKind string

const (
	TimelineUser      TimelineItemKind = "user"
	TimelineAssistant TimelineItemKind = "assistant"
	TimelineActivity  TimelineItemKind = "activity"
)

type QueueItem struct {
	MessageID string `json:"message_id"`
	Message   string `json:"message"`
}
type TimelineItem struct {
	Kind      TimelineItemKind `json:"kind"`
	MessageID string           `json:"message_id,omitempty"`
	Text      string           `json:"text"`
	CreatedAt time.Time        `json:"created_at"`
}
type ArchiveItem struct {
	ArchiveID string       `json:"archive_id"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
	Provider  ProviderName `json:"provider"`
	Model     string       `json:"model,omitempty"`
	Preview   string       `json:"preview,omitempty"`
}

type EventPayload interface {
	EventType() EventType
	validate() error
}

type SnapshotPayload struct {
	Lifecycle    LifecycleState `json:"lifecycle"`
	Queue        []QueueItem    `json:"queue"`
	ContextState ContextState   `json:"context_state"`
}

func (SnapshotPayload) EventType() EventType { return EventSnapshot }
func (p SnapshotPayload) validate() error {
	if p.Queue == nil || !validLifecycle(p.Lifecycle) || !validContextState(p.ContextState) {
		return invalid(nil)
	}
	return ValidateQueue(p.Queue)
}

type CommandResultPayload struct {
	CommandID string        `json:"command_id"`
	Status    CommandStatus `json:"status"`
	Error     *BrowserError `json:"error,omitempty"`
}

func (CommandResultPayload) EventType() EventType { return EventCommandResult }
func (p CommandResultPayload) validate() error {
	if !validID(p.CommandID) || (p.Status != CommandSucceeded && p.Status != CommandRejected) || (p.Status == CommandSucceeded && p.Error != nil) || (p.Status == CommandRejected && (p.Error == nil || !p.Error.valid())) {
		return invalid(nil)
	}
	return nil
}

type TimelinePayload struct {
	Items   []TimelineItem `json:"items"`
	HasMore bool           `json:"has_more"`
}

func (TimelinePayload) EventType() EventType { return EventTimeline }
func (p TimelinePayload) validate() error {
	if p.Items == nil || len(p.Items) > MaxPageSize {
		return invalid(nil)
	}
	for _, item := range p.Items {
		if !validTimelineItem(item) {
			return invalid(nil)
		}
	}
	return nil
}

type HistoryPayload struct {
	Items   []ArchiveItem `json:"items"`
	HasMore bool          `json:"has_more"`
}

func (HistoryPayload) EventType() EventType { return EventHistory }
func (p HistoryPayload) validate() error {
	if p.Items == nil || len(p.Items) > MaxPageSize {
		return invalid(nil)
	}
	for _, item := range p.Items {
		if !validArchiveItem(item) {
			return invalid(nil)
		}
	}
	return nil
}

type UserMessagePayload struct {
	MessageID string    `json:"message_id"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

func (UserMessagePayload) EventType() EventType { return EventUserMessage }
func (p UserMessagePayload) validate() error {
	if !validID(p.MessageID) || !validMessage(p.Text) || p.CreatedAt.IsZero() {
		return invalid(nil)
	}
	return nil
}

type AssistantDeltaPayload struct {
	MessageID string `json:"message_id"`
	Text      string `json:"text"`
}

func (AssistantDeltaPayload) EventType() EventType { return EventAssistantDelta }
func (p AssistantDeltaPayload) validate() error {
	if !validID(p.MessageID) || !validBoundedText(p.Text, MaxDeltaBytes, true) {
		return invalid(nil)
	}
	return nil
}

type AssistantMessagePayload struct {
	MessageID string    `json:"message_id"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

func (AssistantMessagePayload) EventType() EventType { return EventAssistantMessage }
func (p AssistantMessagePayload) validate() error {
	if !validID(p.MessageID) || !validMessage(p.Text) || p.CreatedAt.IsZero() {
		return invalid(nil)
	}
	return nil
}

type QueuePayload struct {
	Items []QueueItem `json:"items"`
}

func (QueuePayload) EventType() EventType { return EventQueue }
func (p QueuePayload) validate() error {
	if p.Items == nil {
		return invalid(nil)
	}
	return ValidateQueue(p.Items)
}

type LifecyclePayload struct {
	State LifecycleState `json:"state"`
}

func (LifecyclePayload) EventType() EventType { return EventLifecycle }
func (p LifecyclePayload) validate() error {
	if !validLifecycle(p.State) {
		return invalid(nil)
	}
	return nil
}

type ProviderPayload struct {
	Provider ProviderName  `json:"provider"`
	State    ProviderState `json:"state"`
	Model    string        `json:"model,omitempty"`
}

func (ProviderPayload) EventType() EventType { return EventProvider }
func (p ProviderPayload) validate() error {
	if p.Provider != ProviderPi || !validProviderState(p.State) || !validBoundedText(p.Model, MaxTitleBytes, false) {
		return invalid(nil)
	}
	return nil
}

type ContextPayload struct {
	Digest string       `json:"digest"`
	State  ContextState `json:"state"`
}

func (ContextPayload) EventType() EventType { return EventContext }
func (p ContextPayload) validate() error {
	if !validDigest(p.Digest) || !validContextState(p.State) {
		return invalid(nil)
	}
	return nil
}

type ActivityPayload struct {
	Kind    ActivityKind `json:"kind"`
	Summary string       `json:"summary"`
}

func (ActivityPayload) EventType() EventType { return EventActivity }
func (p ActivityPayload) validate() error {
	if !validActivity(p.Kind) || !validMessage(p.Summary) {
		return invalid(nil)
	}
	return nil
}

type BlockedPayload struct{ kind BlockedKind }

func NewBlockedPayload(kind BlockedKind) BlockedPayload {
	if _, exists := blockedMessages[kind]; !exists {
		return BlockedPayload{}
	}
	return BlockedPayload{kind: kind}
}
func (p BlockedPayload) Kind() BlockedKind  { return p.kind }
func (p BlockedPayload) Message() string    { return blockedMessages[p.kind] }
func (BlockedPayload) EventType() EventType { return EventBlocked }
func (p BlockedPayload) validate() error {
	if _, exists := blockedMessages[p.kind]; !exists {
		return invalid(nil)
	}
	return nil
}
func (p BlockedPayload) MarshalJSON() ([]byte, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Kind    BlockedKind `json:"kind"`
		Message string      `json:"message"`
	}{p.kind, blockedMessages[p.kind]})
}
func (p *BlockedPayload) UnmarshalJSON(data []byte) error {
	if err := inspectJSON(data, map[string]bool{}); err != nil {
		return err
	}
	var wire struct {
		Kind    BlockedKind `json:"kind"`
		Message string      `json:"message"`
	}
	if err := strictDecode(data, &wire); err != nil {
		return err
	}
	message, exists := blockedMessages[wire.Kind]
	if !exists || wire.Message != message {
		return invalid(nil)
	}
	p.kind = wire.Kind
	return nil
}

var blockedMessages = map[BlockedKind]string{
	BlockedTool:       "A provider tool request was blocked by content-only policy.",
	BlockedPermission: "A provider permission request was blocked by content-only policy.",
}

type ErrorPayload struct {
	Error BrowserError `json:"error"`
}

func (ErrorPayload) EventType() EventType { return EventError }
func (p ErrorPayload) validate() error {
	if !p.Error.valid() {
		return invalid(nil)
	}
	return nil
}

type CompletionPayload struct {
	TurnID string `json:"turn_id"`
}

func (CompletionPayload) EventType() EventType { return EventCompletion }
func (p CompletionPayload) validate() error {
	if !validID(p.TurnID) {
		return invalid(nil)
	}
	return nil
}

type InterruptionPayload struct {
	TurnID string             `json:"turn_id"`
	Reason InterruptionReason `json:"reason"`
}

func (InterruptionPayload) EventType() EventType { return EventInterruption }
func (p InterruptionPayload) validate() error {
	if !validID(p.TurnID) || !validInterruption(p.Reason) {
		return invalid(nil)
	}
	return nil
}

type ArchivePayload struct {
	Action    ArchiveAction `json:"action"`
	ArchiveID string        `json:"archive_id"`
}

func (ArchivePayload) EventType() EventType { return EventArchive }
func (p ArchivePayload) validate() error {
	if !validArchiveAction(p.Action) || !validID(p.ArchiveID) {
		return invalid(nil)
	}
	return nil
}

type Event struct {
	APIVersion     string
	EventID        string
	ConversationID string
	Type           EventType
	Timestamp      time.Time
	Payload        EventPayload
}

type eventWire struct {
	APIVersion     string          `json:"api_version"`
	EventID        string          `json:"event_id"`
	ConversationID string          `json:"conversation_id"`
	Type           EventType       `json:"type"`
	Timestamp      time.Time       `json:"timestamp"`
	Payload        json.RawMessage `json:"payload"`
}

func (event Event) MarshalJSON() ([]byte, error) { return EncodeEvent(event) }

type eventMarshalWire struct {
	APIVersion     string       `json:"api_version"`
	EventID        string       `json:"event_id"`
	ConversationID string       `json:"conversation_id"`
	Type           EventType    `json:"type"`
	Timestamp      time.Time    `json:"timestamp"`
	Payload        EventPayload `json:"payload"`
}

func EncodeEvent(event Event) ([]byte, error) {
	encoded, err := json.Marshal(eventMarshalWire{event.APIVersion, event.EventID, event.ConversationID, event.Type, event.Timestamp, event.Payload})
	if err != nil {
		return nil, invalid(err)
	}
	if len(encoded) > MaxEventBytes {
		return nil, ErrMessageTooLarge
	}
	if err := validateEvent(event); err != nil {
		return nil, err
	}
	return encoded, nil
}

func DecodeEvent(data []byte) (Event, error) {
	if len(data) > MaxEventBytes {
		return Event{}, ErrMessageTooLarge
	}
	if err := inspectJSON(data, map[string]bool{}); err != nil {
		return Event{}, err
	}
	var wire eventWire
	if err := strictDecode(data, &wire); err != nil {
		return Event{}, invalid(err)
	}
	if err := requireFields(data, "api_version", "event_id", "conversation_id", "type", "timestamp", "payload"); err != nil {
		return Event{}, invalid(err)
	}
	payload, err := decodeEventPayload(wire.Type, wire.Payload)
	if err != nil {
		return Event{}, err
	}
	event := Event{wire.APIVersion, wire.EventID, wire.ConversationID, wire.Type, wire.Timestamp, payload}
	if err := validateEvent(event); err != nil {
		return Event{}, err
	}
	return event, nil
}

func decodeEventPayload(kind EventType, raw json.RawMessage) (EventPayload, error) {
	var target EventPayload
	var required []string
	switch kind {
	case EventSnapshot:
		target, required = &SnapshotPayload{}, []string{"lifecycle", "queue", "context_state"}
	case EventCommandResult:
		target, required = &CommandResultPayload{}, []string{"command_id", "status"}
	case EventTimeline:
		target, required = &TimelinePayload{}, []string{"items", "has_more"}
	case EventHistory:
		target, required = &HistoryPayload{}, []string{"items", "has_more"}
	case EventUserMessage:
		target, required = &UserMessagePayload{}, []string{"message_id", "text", "created_at"}
	case EventAssistantDelta:
		target, required = &AssistantDeltaPayload{}, []string{"message_id", "text"}
	case EventAssistantMessage:
		target, required = &AssistantMessagePayload{}, []string{"message_id", "text", "created_at"}
	case EventQueue:
		target, required = &QueuePayload{}, []string{"items"}
	case EventLifecycle:
		target, required = &LifecyclePayload{}, []string{"state"}
	case EventProvider:
		target, required = &ProviderPayload{}, []string{"provider", "state"}
	case EventContext:
		target, required = &ContextPayload{}, []string{"digest", "state"}
	case EventActivity:
		target, required = &ActivityPayload{}, []string{"kind", "summary"}
	case EventBlocked:
		target, required = &BlockedPayload{}, []string{"kind", "message"}
	case EventError:
		target, required = &ErrorPayload{}, []string{"error"}
	case EventCompletion:
		target, required = &CompletionPayload{}, []string{"turn_id"}
	case EventInterruption:
		target, required = &InterruptionPayload{}, []string{"turn_id", "reason"}
	case EventArchive:
		target, required = &ArchivePayload{}, []string{"action", "archive_id"}
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
	case *SnapshotPayload:
		return *value, nil
	case *CommandResultPayload:
		return *value, nil
	case *TimelinePayload:
		return *value, nil
	case *HistoryPayload:
		return *value, nil
	case *UserMessagePayload:
		return *value, nil
	case *AssistantDeltaPayload:
		return *value, nil
	case *AssistantMessagePayload:
		return *value, nil
	case *QueuePayload:
		return *value, nil
	case *LifecyclePayload:
		return *value, nil
	case *ProviderPayload:
		return *value, nil
	case *ContextPayload:
		return *value, nil
	case *ActivityPayload:
		return *value, nil
	case *BlockedPayload:
		return *value, nil
	case *ErrorPayload:
		return *value, nil
	case *CompletionPayload:
		return *value, nil
	case *InterruptionPayload:
		return *value, nil
	case *ArchivePayload:
		return *value, nil
	default:
		panic("unreachable event payload")
	}
}

func validateEvent(event Event) error {
	if event.APIVersion != APIVersion || !validID(event.EventID) || !validID(event.ConversationID) || event.Timestamp.IsZero() || event.Payload == nil || event.Payload.EventType() != event.Type {
		return invalid(nil)
	}
	return event.Payload.validate()
}

func ValidateQueue(items []QueueItem) error {
	if len(items) > MaxQueueItems {
		return ErrMessageTooLarge
	}
	total := 0
	for _, item := range items {
		total += len(item.Message)
		if total > MaxQueueBytes {
			return ErrMessageTooLarge
		}
	}
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if !validID(item.MessageID) || !validMessage(item.Message) {
			return invalid(nil)
		}
		if _, exists := seen[item.MessageID]; exists {
			return invalid(nil)
		}
		seen[item.MessageID] = struct{}{}
	}
	return nil
}

func ValidateReplay(events []Event) error {
	if len(events) > MaxReplayEvents {
		return ErrMessageTooLarge
	}
	total := 0
	seen := make(map[string]struct{}, len(events))
	for _, event := range events {
		encoded, err := EncodeEvent(event)
		if err != nil {
			return err
		}
		if _, exists := seen[event.EventID]; exists {
			return invalid(nil)
		}
		seen[event.EventID] = struct{}{}
		total += len(encoded)
		if total > MaxReplayBytes {
			return ErrMessageTooLarge
		}
	}
	return nil
}

func validLifecycle(value LifecycleState) bool {
	return value == LifecycleConnecting || value == LifecycleReady || value == LifecycleResponding || value == LifecycleInterrupted || value == LifecycleUnavailable
}
func validContextState(value ContextState) bool {
	return value == ContextPending || value == ContextAccepted || value == ContextUnchanged || value == ContextUnavailable
}
func validProviderState(value ProviderState) bool {
	return value == ProviderStarting || value == ProviderReady || value == ProviderUnavailable || value == ProviderRecovering
}
func validActivity(value ActivityKind) bool {
	return value == ActivityStatus || value == ActivityVisibleSummary || value == ActivityRetry || value == ActivityCompaction
}
func validInterruption(value InterruptionReason) bool {
	return value == InterruptionRequested || value == InterruptionProviderExit || value == InterruptionShutdown
}
func validArchiveAction(value ArchiveAction) bool {
	return value == ArchiveCreated || value == ArchiveRestored || value == ArchiveDeleted
}
func validTimelineItem(item TimelineItem) bool {
	if item.CreatedAt.IsZero() || !validMessage(item.Text) {
		return false
	}
	switch item.Kind {
	case TimelineUser, TimelineAssistant:
		return validID(item.MessageID)
	case TimelineActivity:
		return item.MessageID == ""
	default:
		return false
	}
}
func validArchiveItem(item ArchiveItem) bool {
	return validID(item.ArchiveID) && !item.CreatedAt.IsZero() && !item.UpdatedAt.IsZero() && !item.UpdatedAt.Before(item.CreatedAt) && item.Provider == ProviderPi && validBoundedText(item.Model, MaxTitleBytes, false) && validBoundedText(item.Preview, MaxTitleBytes, false) && utf8.ValidString(item.Preview)
}
