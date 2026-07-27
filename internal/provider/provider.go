package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edocsss/agent-whiteboard/internal/common"
)

type Name string

const NamePi Name = "pi"

type AccessMode string

const AccessContentOnly AccessMode = "content-only"

type ReadinessState string

const (
	Ready                  ReadinessState = "ready"
	MissingExecutable      ReadinessState = "missing_executable"
	AuthenticationRequired ReadinessState = "authentication_required"
	StartupFailed          ReadinessState = "startup_failed"
	NoUsableModel          ReadinessState = "no_usable_model"
	ContentOnlyUnavailable ReadinessState = "content_only_unavailable"
	ProtocolIncompatible   ReadinessState = "protocol_incompatible"
)

func (state ReadinessState) Valid() bool {
	switch state {
	case Ready, MissingExecutable, AuthenticationRequired, StartupFailed, NoUsableModel, ContentOnlyUnavailable, ProtocolIncompatible:
		return true
	default:
		return false
	}
}

type Readiness struct {
	State    ReadinessState
	Provider Name
	Model    string
}

func (r Readiness) Validate() error {
	if !r.State.Valid() || r.Provider != NamePi || !utf8.ValidString(r.Model) || (r.State == Ready && r.Model == "") {
		return errors.New("invalid provider readiness")
	}
	return nil
}

type NativeSessionRef struct{ value string }

func NewNativeSessionRef(value string) (NativeSessionRef, error) {
	if value == "" || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
		return NativeSessionRef{}, errors.New("invalid native session reference")
	}
	return NativeSessionRef{value: value}, nil
}
func (r NativeSessionRef) Value() string { return r.value }
func (r NativeSessionRef) Valid() bool {
	return r.value != "" && utf8.ValidString(r.value) && !strings.ContainsRune(r.value, '\x00')
}
func (NativeSessionRef) MarshalJSON() ([]byte, error) {
	return nil, errors.New("native session references cannot be serialized")
}

type OpenRequest struct {
	Provider      Name
	Access        AccessMode
	NativeSession NativeSessionRef
}

func (r OpenRequest) Validate() error {
	if r.Provider != NamePi || r.Access != AccessContentOnly {
		return errors.New("invalid provider open request")
	}
	return nil
}

type LaunchRequest struct {
	Provider      Name
	Access        AccessMode
	NativeSession NativeSessionRef
}

func (r LaunchRequest) Validate() error {
	if r.Provider != NamePi || r.Access != AccessContentOnly {
		return errors.New("invalid provider launch request")
	}
	return nil
}

type Driver interface {
	Readiness(context.Context) Readiness
	Open(context.Context, OpenRequest) (Session, error)
	Delete(context.Context, NativeSessionRef) error
}

type Session interface {
	NativeSession() NativeSessionRef
	Model() string
	History(context.Context, HistoryRequest) (HistoryPage, error)
	// Submit returns only after the native provider has accepted the turn.
	Submit(context.Context, TurnRequest) (AcceptedTurn, error)
	Events() <-chan Event
	Interrupt(context.Context, AcceptedTurn) error
	Reconcile(context.Context, AcceptedTurn) (TurnState, error)
	Shutdown(context.Context) error
}

type Launcher interface {
	Launch(context.Context, LaunchRequest) (ManagedChild, error)
}
type ManagedChild interface {
	Input() io.WriteCloser
	Output() io.Reader
	Errors() io.Reader
	Wait() error
	RequestShutdown(context.Context) error
	Terminate() error
	Kill() error
}

type ResourceKind string

const ResourceMarkdown ResourceKind = "markdown"

type Resource struct {
	Kind      ResourceKind
	ID        string
	CreatedAt time.Time
	UpdatedAt time.Time
	ExpiresAt *time.Time
}

type ContextRevision string

const (
	ContextInitial     ContextRevision = "initial"
	ContextReplacement ContextRevision = "replacement"
)

type PageContext struct {
	Revision       ContextRevision
	Markdown       []byte
	CreatorContext []byte
	Title          string
	URL            string
	Resource       Resource
	Digest         string
}

func (PageContext) MarshalJSON() ([]byte, error) {
	return nil, errors.New("page context is memory-only and cannot be serialized")
}

func (p PageContext) Validate() error {
	if p.Revision != ContextInitial && p.Revision != ContextReplacement {
		return errors.New("invalid context revision")
	}
	if len(p.Markdown) == 0 || len(p.CreatorContext) == 0 || !utf8.Valid(p.Markdown) || !utf8.Valid(p.CreatorContext) || p.Title == "" || !utf8.ValidString(p.Title) || !validURL(p.URL) || !validResource(p.Resource) || !validDigest(p.Digest) {
		return errors.New("invalid page context")
	}
	return nil
}

type TurnRequest struct {
	TurnID  string
	Message string
	Context *PageContext
}

func (r TurnRequest) Validate() error {
	if !validID(r.TurnID) || r.Message == "" || !utf8.ValidString(r.Message) {
		return errors.New("invalid turn request")
	}
	if r.Context != nil {
		return r.Context.Validate()
	}
	return nil
}

type AcceptedTurn struct {
	TurnID     string
	AcceptedAt time.Time
}

func (a AcceptedTurn) Validate() error {
	if !validID(a.TurnID) || a.AcceptedAt.IsZero() {
		return errors.New("invalid accepted turn")
	}
	return nil
}

type TurnState string

const (
	TurnAccepted    TurnState = "accepted"
	TurnRunning     TurnState = "running"
	TurnCompleted   TurnState = "completed"
	TurnInterrupted TurnState = "interrupted"
	TurnUnknown     TurnState = "unknown"
)

func (state TurnState) Valid() bool {
	switch state {
	case TurnAccepted, TurnRunning, TurnCompleted, TurnInterrupted, TurnUnknown:
		return true
	default:
		return false
	}
}

type HistoryRole string

const (
	HistoryUser      HistoryRole = "user"
	HistoryAssistant HistoryRole = "assistant"
)

type HistoryItem struct {
	MessageID string
	Role      HistoryRole
	Text      string
	CreatedAt time.Time
}
type HistoryRequest struct {
	BeforeMessageID string
	Limit           int
}

func (r HistoryRequest) Validate() error {
	if r.BeforeMessageID != "" && !validID(r.BeforeMessageID) {
		return errors.New("invalid history cursor")
	}
	if r.Limit < 0 || r.Limit > 100 {
		return errors.New("invalid history limit")
	}
	return nil
}

type HistoryPage struct {
	Items   []HistoryItem
	HasMore bool
}

func (p HistoryPage) Validate() error {
	if len(p.Items) > 100 {
		return errors.New("invalid history page")
	}
	for _, item := range p.Items {
		if !validID(item.MessageID) || (item.Role != HistoryUser && item.Role != HistoryAssistant) || item.Text == "" || !utf8.ValidString(item.Text) || item.CreatedAt.IsZero() {
			return errors.New("invalid history item")
		}
	}
	return nil
}

type EventKind string

const (
	EventUserMessage      EventKind = "user_message"
	EventAssistantDelta   EventKind = "assistant_delta"
	EventAssistantMessage EventKind = "assistant_message"
	EventActivity         EventKind = "activity"
	EventBlocked          EventKind = "blocked"
	EventCompletion       EventKind = "completion"
	EventInterruption     EventKind = "interruption"
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

type Event struct {
	Kind         EventKind
	MessageID    string
	TurnID       string
	Text         string
	Timestamp    time.Time
	Activity     ActivityKind
	Blocked      BlockedKind
	Interruption InterruptionReason
}

func NewUserMessageEvent(messageID, text string, at time.Time) Event {
	return Event{Kind: EventUserMessage, MessageID: messageID, Text: text, Timestamp: at}
}
func NewAssistantDeltaEvent(messageID, text string) Event {
	return Event{Kind: EventAssistantDelta, MessageID: messageID, Text: text}
}
func NewAssistantMessageEvent(messageID, text string, at time.Time) Event {
	return Event{Kind: EventAssistantMessage, MessageID: messageID, Text: text, Timestamp: at}
}
func NewActivityEvent(kind ActivityKind, summary string) Event {
	return Event{Kind: EventActivity, Activity: kind, Text: summary}
}
func NewBlockedEvent(kind BlockedKind) Event {
	return Event{Kind: EventBlocked, Blocked: kind, Text: blockedMessage(kind)}
}
func NewCompletionEvent(turnID string) Event { return Event{Kind: EventCompletion, TurnID: turnID} }
func NewInterruptionEvent(turnID string, reason InterruptionReason) Event {
	return Event{Kind: EventInterruption, TurnID: turnID, Interruption: reason}
}
func (e Event) Validate() error {
	switch e.Kind {
	case EventUserMessage, EventAssistantMessage:
		if !validID(e.MessageID) || e.Text == "" || !utf8.ValidString(e.Text) || e.Timestamp.IsZero() {
			return errors.New("invalid provider message event")
		}
	case EventAssistantDelta:
		if !validID(e.MessageID) || e.Text == "" || !utf8.ValidString(e.Text) || len(e.Text) > 32<<10 {
			return errors.New("invalid provider delta event")
		}
	case EventActivity:
		if !validActivity(e.Activity) || e.Text == "" || !utf8.ValidString(e.Text) {
			return errors.New("invalid provider activity event")
		}
	case EventBlocked:
		if message := blockedMessage(e.Blocked); message == "" || e.Text != message {
			return errors.New("invalid provider blocked event")
		}
	case EventCompletion:
		if !validID(e.TurnID) {
			return errors.New("invalid provider completion event")
		}
	case EventInterruption:
		if !validID(e.TurnID) || !validInterruption(e.Interruption) {
			return errors.New("invalid provider interruption event")
		}
	default:
		return errors.New("invalid provider event kind")
	}
	return nil
}
func (Event) MarshalJSON() ([]byte, error) {
	return nil, errors.New("provider events cannot cross the browser wire boundary")
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
func validURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil
}
func validResource(resource Resource) bool {
	return resource.Kind == ResourceMarkdown && validID(resource.ID) && !resource.CreatedAt.IsZero() && !resource.UpdatedAt.IsZero() && !resource.UpdatedAt.Before(resource.CreatedAt) && (resource.ExpiresAt == nil || !resource.ExpiresAt.Before(resource.CreatedAt))
}
func validActivity(value ActivityKind) bool {
	return value == ActivityStatus || value == ActivityVisibleSummary || value == ActivityRetry || value == ActivityCompaction
}
func validInterruption(value InterruptionReason) bool {
	return value == InterruptionRequested || value == InterruptionProviderExit || value == InterruptionShutdown
}
func blockedMessage(kind BlockedKind) string {
	switch kind {
	case BlockedTool:
		return "A provider tool request was blocked by content-only policy."
	case BlockedPermission:
		return "A provider permission request was blocked by content-only policy."
	default:
		return ""
	}
}

var _ json.Marshaler = NativeSessionRef{}
var _ json.Marshaler = PageContext{}
var _ json.Marshaler = Event{}
