package protocol

import (
	"encoding/json"
	"reflect"
	"time"
	"unicode/utf8"
)

type EventType string

const (
	EventSnapshot            EventType = "snapshot"
	EventCommandResult       EventType = "command_result"
	EventTimeline            EventType = "timeline"
	EventHistory             EventType = "history"
	EventUserMessage         EventType = "user_message"
	EventAssistantDelta      EventType = "assistant_delta"
	EventAssistantMessage    EventType = "assistant_message"
	EventQueue               EventType = "queue"
	EventLifecycle           EventType = "lifecycle"
	EventProvider            EventType = "provider"
	EventContext             EventType = "context"
	EventActivity            EventType = "activity"
	EventBlocked             EventType = "blocked"
	EventError               EventType = "error"
	EventCompletion          EventType = "completion"
	EventInterruption        EventType = "interruption"
	EventArchive             EventType = "archive"
	EventToolActivity        EventType = "tool_activity"
	EventInteractionRequest  EventType = "interaction_request"
	EventInteractionResolved EventType = "interaction_resolved"
	EventSettings            EventType = "settings"
	EventSkillCatalog        EventType = "skill_catalog"
	EventCompaction          EventType = "compaction"
)

type LifecycleState string

const (
	LifecycleConnecting  LifecycleState = "connecting"
	LifecycleReady       LifecycleState = "ready"
	LifecycleResponding  LifecycleState = "responding"
	LifecycleCompacting  LifecycleState = "compacting"
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
	TurnID    string                      `json:"turn_id"`
	MessageID string                      `json:"message_id"`
	Content   MessageContent              `json:"content"`
	Images    []ImageDescriptor           `json:"images,omitempty"`
	Settings  *PresentedExecutionSettings `json:"settings"`
}

func (item *QueueItem) UnmarshalJSON(data []byte) error {
	if err := inspectJSON(data, map[string]bool{"settings": true}); err != nil {
		return err
	}
	if err := requireFields(data, "turn_id", "message_id", "content", "settings"); err != nil {
		return err
	}
	type wire QueueItem
	var decoded wire
	if err := strictDecode(data, &decoded); err != nil {
		return err
	}
	*item = QueueItem(decoded)
	return nil
}

type TimelineItem struct {
	ItemID    string            `json:"item_id"`
	Kind      TimelineItemKind  `json:"kind"`
	TurnID    string            `json:"turn_id,omitempty"`
	MessageID string            `json:"message_id,omitempty"`
	Text      string            `json:"text,omitempty"`
	Content   *MessageContent   `json:"content,omitempty"`
	Images    []ImageDescriptor `json:"images,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
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
	Lifecycle         LifecycleState              `json:"lifecycle"`
	Queue             []QueueItem                 `json:"queue"`
	ContextState      ContextState                `json:"context_state"`
	ActiveWork        *ActiveWork                 `json:"active_work"`
	SupportsImages    bool                        `json:"supports_images"`
	SettingsState     *SettingsState              `json:"settings_state"`
	EffectiveSettings *PresentedExecutionSettings `json:"effective_settings"`
	Catalog           []CatalogModel              `json:"catalog"`
	SkillsState       *SkillsState                `json:"skills_state"`
	Skills            []SkillDescriptor           `json:"skills"`
	MaxSelectedSkills *int                        `json:"max_selected_skills"`
	SupportsCompact   bool                        `json:"supports_compact"`
	BusyPolicy        BusyTurnPolicy              `json:"busy_policy"`
	ComposerAdmission ComposerAdmission           `json:"composer_admission"`
}

func (SnapshotPayload) EventType() EventType { return EventSnapshot }
func (p SnapshotPayload) validate() error {
	if p.Queue == nil || p.Skills == nil || !validLifecycle(p.Lifecycle) || !validContextState(p.ContextState) || !validActiveWork(p.Lifecycle, p.ActiveWork) || validateSettingsSnapshot(p.SettingsState, p.EffectiveSettings, p.Catalog) != nil ||
		p.SkillsState == nil || validateSkillCatalog(*p.SkillsState, p.Skills, p.MaxSelectedSkills) != nil || !validComposerAdmission(p.BusyPolicy, p.ComposerAdmission) {
		return invalid(nil)
	}
	if p.EffectiveSettings != nil {
		if supportsImages, known := selectableModelCapabilities(*p.EffectiveSettings, p.Catalog); known && supportsImages != p.SupportsImages {
			return invalid(nil)
		}
	}
	return ValidateQueue(p.Queue)
}

func (p SnapshotPayload) ValidateForProvider(provider ProviderName) error {
	if !provider.Valid() || p.validate() != nil {
		return invalid(nil)
	}
	return nil
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
	CommandID  string         `json:"command_id"`
	Items      []TimelineItem `json:"items"`
	NextCursor *string        `json:"next_cursor"`
}

func (TimelinePayload) EventType() EventType { return EventTimeline }
func (p TimelinePayload) validate() error {
	if !validID(p.CommandID) || p.Items == nil || len(p.Items) > MaxPageSize || !validCursor(p.NextCursor) {
		return invalid(nil)
	}
	total := 0
	seen := make(map[string]struct{}, len(p.Items))
	for _, item := range p.Items {
		if !validTimelineItem(item) {
			return invalid(nil)
		}
		if item.Content != nil {
			total += item.Content.SemanticBytes()
		} else {
			total += len(item.Text)
		}
		if total > MaxTimelineBytes {
			return ErrMessageTooLarge
		}
		if _, exists := seen[item.ItemID]; exists {
			return invalid(nil)
		}
		seen[item.ItemID] = struct{}{}
	}
	if p.NextCursor != nil && (len(p.Items) == 0 || *p.NextCursor != p.Items[len(p.Items)-1].ItemID) {
		return invalid(nil)
	}
	return nil
}

type HistoryPayload struct {
	CommandID  string        `json:"command_id"`
	Items      []ArchiveItem `json:"items"`
	NextCursor *string       `json:"next_cursor"`
}

func (HistoryPayload) EventType() EventType { return EventHistory }
func (p HistoryPayload) validate() error {
	if !validID(p.CommandID) || p.Items == nil || len(p.Items) > MaxPageSize || !validCursor(p.NextCursor) {
		return invalid(nil)
	}
	seen := make(map[string]struct{}, len(p.Items))
	for _, item := range p.Items {
		if !validArchiveItem(item) {
			return invalid(nil)
		}
		if _, exists := seen[item.ArchiveID]; exists {
			return invalid(nil)
		}
		seen[item.ArchiveID] = struct{}{}
	}
	if p.NextCursor != nil && (len(p.Items) == 0 || *p.NextCursor != p.Items[len(p.Items)-1].ArchiveID) {
		return invalid(nil)
	}
	return nil
}

type UserMessagePayload struct {
	TurnID    string            `json:"turn_id"`
	MessageID string            `json:"message_id"`
	Content   MessageContent    `json:"content"`
	Images    []ImageDescriptor `json:"images,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

func (UserMessagePayload) EventType() EventType { return EventUserMessage }
func (p UserMessagePayload) validate() error {
	if !validID(p.TurnID) || !validID(p.MessageID) || !validContentWithDescriptors(p.Content, p.Images) || p.CreatedAt.IsZero() {
		return invalid(nil)
	}
	return nil
}

type AssistantDeltaPayload struct {
	TurnID    string `json:"turn_id"`
	MessageID string `json:"message_id"`
	Text      string `json:"text"`
}

func (AssistantDeltaPayload) EventType() EventType { return EventAssistantDelta }
func (p AssistantDeltaPayload) validate() error {
	if !validID(p.TurnID) || !validID(p.MessageID) || !validBoundedText(p.Text, MaxDeltaBytes, true) {
		return invalid(nil)
	}
	return nil
}

type AssistantMessagePayload struct {
	TurnID    string    `json:"turn_id"`
	MessageID string    `json:"message_id"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

func (AssistantMessagePayload) EventType() EventType { return EventAssistantMessage }
func (p AssistantMessagePayload) validate() error {
	if !validID(p.TurnID) || !validID(p.MessageID) || !validMessage(p.Text) || p.CreatedAt.IsZero() {
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
	State      LifecycleState `json:"state"`
	ActiveWork *ActiveWork    `json:"active_work"`
}

func (LifecyclePayload) EventType() EventType { return EventLifecycle }
func (p LifecyclePayload) validate() error {
	if !validLifecycle(p.State) || !validActiveWork(p.State, p.ActiveWork) {
		return invalid(nil)
	}
	return nil
}

type ProviderPayload struct {
	Provider       ProviderName  `json:"provider"`
	State          ProviderState `json:"state"`
	Model          string        `json:"model,omitempty"`
	SupportsImages bool          `json:"supports_images"`
}

func (ProviderPayload) EventType() EventType { return EventProvider }
func (p ProviderPayload) validate() error {
	if !p.Provider.Valid() || !validProviderState(p.State) || !validBoundedText(p.Model, MaxTitleBytes, false) || (p.State == ProviderReady && p.Model == "") {
		return invalid(nil)
	}
	return nil
}

type SettingsPayload struct {
	SettingsState     SettingsState               `json:"settings_state"`
	EffectiveSettings *PresentedExecutionSettings `json:"effective_settings"`
	Catalog           []CatalogModel              `json:"catalog"`
	AcceptedTurnID    *string                     `json:"accepted_turn_id"`
}

func (SettingsPayload) EventType() EventType { return EventSettings }
func (p SettingsPayload) validate() error {
	if !p.SettingsState.Valid() || validateCatalog(p.Catalog) != nil || (p.AcceptedTurnID != nil && !validID(*p.AcceptedTurnID)) {
		return invalid(nil)
	}
	if p.SettingsState == SettingsVerified {
		if p.EffectiveSettings == nil || p.EffectiveSettings.Validate() != nil || validateSelectableSettings(*p.EffectiveSettings, p.Catalog) != nil {
			return invalid(nil)
		}
		return nil
	}
	if p.EffectiveSettings != nil || p.AcceptedTurnID != nil {
		return invalid(nil)
	}
	return nil
}

type SkillCatalogPayload struct {
	State             SkillsState       `json:"state"`
	Skills            []SkillDescriptor `json:"skills"`
	MaxSelectedSkills *int              `json:"max_selected_skills"`
}

func (SkillCatalogPayload) EventType() EventType { return EventSkillCatalog }
func (p SkillCatalogPayload) validate() error {
	return validateSkillCatalog(p.State, p.Skills, p.MaxSelectedSkills)
}

type CompactionPayload struct {
	WorkID string           `json:"work_id"`
	Status CompactionStatus `json:"status"`
}

func (CompactionPayload) EventType() EventType { return EventCompaction }
func (p CompactionPayload) validate() error {
	if !validID(p.WorkID) || !p.Status.valid() {
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
	if !validActivity(p.Kind) || !validBoundedText(p.Summary, MaxSummaryBytes, true) {
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
	if isNilEventPayload(event.Payload) {
		return nil, invalid(nil)
	}
	encoded, err := marshalApplicationJSON(eventMarshalWire{event.APIVersion, event.EventID, event.ConversationID, event.Type, event.Timestamp, event.Payload})
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
	nullable := map[string]bool{
		"payload.active_work":         true,
		"payload.max_selected_skills": true,
		"payload.local_deadline":      true,
		"payload.skills_state":        true,
		"payload.settings_state":      true,
		"payload.effective_settings":  true,
		"payload.accepted_turn_id":    true,
		"payload.queue[].settings":    true,
		"payload.items[].settings":    true,
		"payload.next_cursor":         true,
		"payload.turn_id":             true,
		"payload.options":             true,
		"payload.questions":           true,
		"payload.fields":              true,
		"payload.questions[].options": true,
		"payload.fields[].options":    true,
	}
	if err := inspectJSON(data, nullable); err != nil {
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
		target, required = &SnapshotPayload{}, []string{"lifecycle", "queue", "context_state", "active_work", "supports_images", "settings_state", "effective_settings", "catalog", "skills_state", "skills", "max_selected_skills", "supports_compact", "busy_policy", "composer_admission"}
	case EventCommandResult:
		target, required = &CommandResultPayload{}, []string{"command_id", "status"}
	case EventTimeline:
		target, required = &TimelinePayload{}, []string{"command_id", "items", "next_cursor"}
	case EventHistory:
		target, required = &HistoryPayload{}, []string{"command_id", "items", "next_cursor"}
	case EventUserMessage:
		target, required = &UserMessagePayload{}, []string{"turn_id", "message_id", "content", "created_at"}
	case EventAssistantDelta:
		target, required = &AssistantDeltaPayload{}, []string{"turn_id", "message_id", "text"}
	case EventAssistantMessage:
		target, required = &AssistantMessagePayload{}, []string{"turn_id", "message_id", "text", "created_at"}
	case EventQueue:
		target, required = &QueuePayload{}, []string{"items"}
	case EventLifecycle:
		target, required = &LifecyclePayload{}, []string{"state", "active_work"}
	case EventProvider:
		target, required = &ProviderPayload{}, []string{"provider", "state", "supports_images"}
	case EventSettings:
		target, required = &SettingsPayload{}, []string{"settings_state", "effective_settings", "catalog", "accepted_turn_id"}
	case EventSkillCatalog:
		target, required = &SkillCatalogPayload{}, []string{"state", "skills", "max_selected_skills"}
	case EventCompaction:
		target, required = &CompactionPayload{}, []string{"work_id", "status"}
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
	case EventToolActivity:
		target, required = &ToolActivityPayload{}, []string{"activity_id", "kind", "status", "title", "summary", "detail"}
	case EventInteractionRequest:
		target, required = &InteractionRequestPayload{}, []string{"request_id", "kind", "title", "summary", "command", "working_directory", "options", "questions", "fields", "local_deadline"}
	case EventInteractionResolved:
		target, required = &InteractionResolvedPayload{}, []string{"request_id", "kind", "option_id"}
	default:
		return nil, invalid(nil)
	}
	if err := strictDecode(raw, target); err != nil {
		return nil, invalid(err)
	}
	if err := requireFields(raw, required...); err != nil {
		return nil, invalid(err)
	}
	if kind == EventInteractionRequest {
		var nested struct {
			Fields []json.RawMessage `json:"fields"`
		}
		if json.Unmarshal(raw, &nested) != nil {
			return nil, invalid(nil)
		}
		for _, field := range nested.Fields {
			if err := requireFields(field, "id", "label", "description", "type", "required", "secret", "multiline", "options"); err != nil {
				return nil, invalid(err)
			}
		}
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
	case *SettingsPayload:
		return *value, nil
	case *SkillCatalogPayload:
		return *value, nil
	case *CompactionPayload:
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
	case *ToolActivityPayload:
		return *value, nil
	case *InteractionRequestPayload:
		return *value, nil
	case *InteractionResolvedPayload:
		return *value, nil
	default:
		panic("unreachable event payload")
	}
}

func validateEvent(event Event) error {
	if event.APIVersion != APIVersion || !validID(event.EventID) || !validID(event.ConversationID) || event.Timestamp.IsZero() || isNilEventPayload(event.Payload) {
		return invalid(nil)
	}
	if event.Payload.EventType() != event.Type {
		return invalid(nil)
	}
	return event.Payload.validate()
}

func isNilEventPayload(payload EventPayload) bool {
	if payload == nil {
		return true
	}
	value := reflect.ValueOf(payload)
	return value.Kind() == reflect.Pointer && value.IsNil()
}

func ValidateQueue(items []QueueItem) error {
	if len(items) > MaxQueueItems {
		return ErrMessageTooLarge
	}
	total := 0
	for _, item := range items {
		total += item.Content.SemanticBytes() + presentedSettingsBytes(item.Settings)
		if total > MaxQueueBytes {
			return ErrMessageTooLarge
		}
	}
	seenMessages := make(map[string]struct{}, len(items))
	seenTurns := make(map[string]struct{}, len(items))
	for _, item := range items {
		if !validID(item.TurnID) || !validID(item.MessageID) || !validContentWithDescriptors(item.Content, item.Images) || (item.Settings != nil && item.Settings.Validate() != nil) {
			return invalid(nil)
		}
		if _, exists := seenMessages[item.MessageID]; exists {
			return invalid(nil)
		}
		if _, exists := seenTurns[item.TurnID]; exists {
			return invalid(nil)
		}
		seenMessages[item.MessageID] = struct{}{}
		seenTurns[item.TurnID] = struct{}{}
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
	return value == LifecycleConnecting || value == LifecycleReady || value == LifecycleResponding || value == LifecycleCompacting || value == LifecycleInterrupted || value == LifecycleUnavailable
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
	if !validID(item.ItemID) || item.CreatedAt.IsZero() {
		return false
	}
	switch item.Kind {
	case TimelineUser:
		return validID(item.TurnID) && validID(item.MessageID) && item.Text == "" && item.Content != nil && validContentWithDescriptors(*item.Content, item.Images)
	case TimelineAssistant:
		return validID(item.TurnID) && validID(item.MessageID) && validMessage(item.Text) && item.Content == nil && len(item.Images) == 0
	case TimelineActivity:
		return item.MessageID == "" && (item.TurnID == "" || validID(item.TurnID)) && validMessage(item.Text) && item.Content == nil && len(item.Images) == 0
	default:
		return false
	}
}
func validTextWithDescriptors(text string, images []ImageDescriptor) bool {
	return validBoundedText(text, MaxMessageBytes, false) && (text != "" || len(images) != 0) && validateImageDescriptors(images) == nil
}
func validContentWithDescriptors(content MessageContent, images []ImageDescriptor) bool {
	return content.ValidateEvent() == nil && (!content.Empty() || len(images) != 0) && validateImageDescriptors(images) == nil
}
func validArchiveItem(item ArchiveItem) bool {
	return validID(item.ArchiveID) && !item.CreatedAt.IsZero() && !item.UpdatedAt.IsZero() && !item.UpdatedAt.Before(item.CreatedAt) && item.Provider.Valid() && validBoundedText(item.Model, MaxTitleBytes, false) && validBoundedText(item.Preview, MaxTitleBytes, false) && utf8.ValidString(item.Preview)
}
func validCursor(cursor *string) bool {
	return cursor == nil || validID(*cursor)
}
