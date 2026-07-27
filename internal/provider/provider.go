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

const (
	MaxTurnMessageBytes     = 64 << 10
	MaxMarkdownBytes        = 10 << 20
	MaxCreatorContextBytes  = 1 << 20
	MaxTitleBytes           = 512
	MaxURLBytes             = 8 << 10
	MaxNativeReferenceBytes = 8 << 10
	MaxHistoryItems         = 100
	MaxHistoryItemBytes     = 64 << 10
	MaxHistoryBytes         = 4 << 20
	MaxEventTextBytes       = 64 << 10
	MaxDeltaBytes           = 32 << 10
	MaxSummaryBytes         = 8 << 10
)

type Name string

const NamePi Name = "pi"

type AccessMode string

const AccessContentOnly AccessMode = "content-only"

type ProviderErrorCode string

const (
	ErrorNotReady               ProviderErrorCode = "not_ready"
	ErrorReadinessFailed        ProviderErrorCode = "readiness_failed"
	ErrorMissingExecutable      ProviderErrorCode = "missing_executable"
	ErrorStartupFailed          ProviderErrorCode = "startup_failed"
	ErrorAuthenticationRequired ProviderErrorCode = "authentication_required"
	ErrorNoUsableModel          ProviderErrorCode = "no_usable_model"
	ErrorContentOnlyUnavailable ProviderErrorCode = "content_only_unavailable"
	ErrorProtocolIncompatible   ProviderErrorCode = "protocol_incompatible"
	ErrorProtocolFailure        ProviderErrorCode = "protocol_failure"
	ErrorMalformedStream        ProviderErrorCode = "malformed_stream"
	ErrorChildExited            ProviderErrorCode = "child_exited"
	ErrorNativeSessionMissing   ProviderErrorCode = "native_session_missing"
	ErrorContextTooLarge        ProviderErrorCode = "context_too_large"
	ErrorAcceptanceUnknown      ProviderErrorCode = "acceptance_unknown"
)

var providerErrorMessages = map[ProviderErrorCode]string{
	ErrorNotReady:               "The provider is not ready.",
	ErrorReadinessFailed:        "The provider readiness check failed.",
	ErrorMissingExecutable:      "The provider executable is unavailable.",
	ErrorStartupFailed:          "The provider could not be started.",
	ErrorAuthenticationRequired: "Provider-native authentication is required.",
	ErrorNoUsableModel:          "The provider has no usable default model.",
	ErrorContentOnlyUnavailable: "The provider cannot enforce content-only access.",
	ErrorProtocolIncompatible:   "The provider protocol is incompatible.",
	ErrorProtocolFailure:        "The provider protocol operation failed.",
	ErrorMalformedStream:        "The provider returned a malformed event stream.",
	ErrorChildExited:            "The provider process stopped unexpectedly.",
	ErrorNativeSessionMissing:   "The native provider session is unavailable.",
	ErrorContextTooLarge:        "The complete turn does not fit the effective model capacity.",
	ErrorAcceptanceUnknown:      "The provider turn acceptance outcome is unknown.",
}

// ProviderError is a closed, provider-neutral failure. It intentionally carries
// no native error text, path, identifier, or protocol payload.
type ProviderError struct{ code ProviderErrorCode }

func NewProviderError(code ProviderErrorCode) ProviderError {
	if _, ok := providerErrorMessages[code]; !ok {
		return ProviderError{}
	}
	return ProviderError{code: code}
}
func (e ProviderError) Code() ProviderErrorCode { return e.code }
func (e ProviderError) Valid() bool {
	_, ok := providerErrorMessages[e.code]
	return ok
}
func (e ProviderError) Error() string {
	if message, ok := providerErrorMessages[e.code]; ok {
		return message
	}
	return "invalid provider error"
}
func AllProviderErrorCodes() []ProviderErrorCode {
	return []ProviderErrorCode{
		ErrorNotReady, ErrorReadinessFailed, ErrorMissingExecutable, ErrorStartupFailed, ErrorAuthenticationRequired,
		ErrorNoUsableModel, ErrorContentOnlyUnavailable, ErrorProtocolIncompatible, ErrorProtocolFailure, ErrorMalformedStream,
		ErrorChildExited, ErrorNativeSessionMissing, ErrorContextTooLarge, ErrorAcceptanceUnknown,
	}
}

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
	if !r.State.Valid() || r.Provider != NamePi || !validBoundedText(r.Model, MaxTitleBytes, false) || (r.State == Ready && r.Model == "") {
		return errors.New("invalid provider readiness")
	}
	return nil
}

type NativeSessionRef struct{ value string }

func NewNativeSessionRef(value string) (NativeSessionRef, error) {
	if !validBoundedText(value, MaxNativeReferenceBytes, true) || strings.ContainsRune(value, '\x00') {
		return NativeSessionRef{}, errors.New("invalid native session reference")
	}
	return NativeSessionRef{value: value}, nil
}
func (r NativeSessionRef) Value() string { return r.value }
func (r NativeSessionRef) Valid() bool {
	return validBoundedText(r.value, MaxNativeReferenceBytes, true) && !strings.ContainsRune(r.value, '\x00')
}
func (NativeSessionRef) MarshalJSON() ([]byte, error) {
	return nil, errors.New("native session references cannot be serialized")
}

// NativeSession is validated provider-owned session metadata. Ref remains
// memory-only and must never be copied to browser protocol values.
type NativeSession struct {
	Ref       NativeSessionRef
	Provider  Name
	Model     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (s NativeSession) Validate() error {
	if !s.Ref.Valid() || s.Provider != NamePi || !validBoundedText(s.Model, MaxTitleBytes, true) || s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() || s.UpdatedAt.Before(s.CreatedAt) {
		return errors.New("invalid native session metadata")
	}
	return nil
}
func (NativeSession) MarshalJSON() ([]byte, error) {
	return nil, errors.New("native session metadata cannot be serialized")
}

type CreateRequest struct {
	Provider Name
	Access   AccessMode
}

func (r CreateRequest) Validate() error { return validateProviderAccess(r.Provider, r.Access) }

type ResumeRequest struct {
	Provider      Name
	Access        AccessMode
	NativeSession NativeSessionRef
}

func (r ResumeRequest) Validate() error {
	if validateProviderAccess(r.Provider, r.Access) != nil || !r.NativeSession.Valid() {
		return errors.New("invalid provider resume request")
	}
	return nil
}

type InspectRequest struct {
	Provider      Name
	NativeSession NativeSessionRef
}

func (r InspectRequest) Validate() error {
	if r.Provider != NamePi || !r.NativeSession.Valid() {
		return errors.New("invalid provider inspect request")
	}
	return nil
}

type DeleteRequest struct {
	Provider      Name
	NativeSession NativeSessionRef
}

func (r DeleteRequest) Validate() error {
	if r.Provider != NamePi || !r.NativeSession.Valid() {
		return errors.New("invalid provider delete request")
	}
	return nil
}

type Driver interface {
	Readiness(context.Context) Readiness
	Create(context.Context, CreateRequest) (Session, error)
	Resume(context.Context, ResumeRequest) (Session, error)
	Inspect(context.Context, InspectRequest) (NativeSession, error)
	Delete(context.Context, DeleteRequest) error
}

type Session interface {
	// NativeSession returns valid metadata for every successfully created or resumed session.
	NativeSession() NativeSession
	Model() string
	History(context.Context, HistoryRequest) (HistoryPage, error)
	// Preflight resolves the effective model and verifies complete turn sizing
	// against the current native session before Submit is attempted.
	Preflight(context.Context, PreflightRequest) (PreflightResult, error)
	// Submit returns only after the native provider has accepted the turn. If
	// acceptance cannot be determined, it returns ErrorAcceptanceUnknown.
	Submit(context.Context, TurnRequest) (AcceptedTurn, error)
	Events() <-chan Event
	Interrupt(context.Context, AcceptedTurn) error
	Reconcile(context.Context, AcceptedTurn) (TurnState, error)
	Shutdown(context.Context) error
}

type LaunchOperation string

const (
	LaunchCreate LaunchOperation = "create"
	LaunchResume LaunchOperation = "resume"
)

type LaunchRequest struct {
	Provider      Name
	Access        AccessMode
	Operation     LaunchOperation
	NativeSession NativeSessionRef
}

func (r LaunchRequest) Validate() error {
	if validateProviderAccess(r.Provider, r.Access) != nil {
		return errors.New("invalid provider launch request")
	}
	switch r.Operation {
	case LaunchCreate:
		if r.NativeSession.Valid() {
			return errors.New("create launch cannot include a native session reference")
		}
	case LaunchResume:
		if !r.NativeSession.Valid() {
			return errors.New("resume launch requires a native session reference")
		}
	default:
		return errors.New("invalid provider launch operation")
	}
	return nil
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
	if !validBoundedBytes(p.Markdown, MaxMarkdownBytes, true) || !validBoundedBytes(p.CreatorContext, MaxCreatorContextBytes, true) ||
		!validBoundedText(p.Title, MaxTitleBytes, true) || !validURL(p.URL) || !validResource(p.Resource) || !validDigest(p.Digest) {
		return errors.New("invalid page context")
	}
	return nil
}

type TurnRequest struct {
	TurnID    string
	MessageID string
	Message   string
	Context   *PageContext
}

func (r TurnRequest) Validate() error {
	if !validID(r.TurnID) || !validID(r.MessageID) || !validBoundedText(r.Message, MaxTurnMessageBytes, true) {
		return errors.New("invalid turn request")
	}
	if r.Context != nil {
		return r.Context.Validate()
	}
	return nil
}

type PreflightRequest struct {
	Turn TurnRequest
}

func (r PreflightRequest) Validate() error {
	if r.Turn.Validate() != nil {
		return errors.New("invalid provider preflight request")
	}
	return nil
}

// PreflightResult reports provider-effective sizing after model resolution.
// Token counts use the selected provider's tokenizer; EffectiveCapacityTokens
// is the usable capacity after the reported safety margin.
type PreflightResult struct {
	ResolvedModel           string
	EstimatedInputTokens    int
	EffectiveCapacityTokens int
	SafetyMarginTokens      int
}

func (r PreflightResult) Validate() error {
	if !validBoundedText(r.ResolvedModel, MaxTitleBytes, true) || r.EstimatedInputTokens <= 0 || r.EffectiveCapacityTokens <= 0 || r.SafetyMarginTokens < 0 || r.EstimatedInputTokens > r.EffectiveCapacityTokens {
		return errors.New("invalid provider preflight result")
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
	TurnID    string
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
	if r.Limit < 0 || r.Limit > MaxHistoryItems {
		return errors.New("invalid history limit")
	}
	return nil
}

type HistoryPage struct {
	Items      []HistoryItem
	NextCursor string
}

func (p HistoryPage) Validate() error {
	if p.NextCursor != "" && !validID(p.NextCursor) {
		return errors.New("invalid history next cursor")
	}
	return validateHistory(p.Items)
}

func validateHistory(items []HistoryItem) error {
	if len(items) > MaxHistoryItems {
		return errors.New("invalid history page")
	}
	total := 0
	for _, item := range items {
		if !validID(item.TurnID) || !validID(item.MessageID) || (item.Role != HistoryUser && item.Role != HistoryAssistant) || !validBoundedText(item.Text, MaxHistoryItemBytes, true) || item.CreatedAt.IsZero() {
			return errors.New("invalid history item")
		}
		total += len(item.Text)
		if total > MaxHistoryBytes {
			return errors.New("provider history exceeds byte limit")
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
	EventTerminalFailure  EventKind = "terminal_failure"
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
	Failure      ProviderError
}

func NewUserMessageEvent(turnID, messageID, text string, at time.Time) Event {
	return Event{Kind: EventUserMessage, TurnID: turnID, MessageID: messageID, Text: text, Timestamp: at}
}
func NewAssistantDeltaEvent(turnID, messageID, text string) Event {
	return Event{Kind: EventAssistantDelta, TurnID: turnID, MessageID: messageID, Text: text}
}
func NewAssistantMessageEvent(turnID, messageID, text string, at time.Time) Event {
	return Event{Kind: EventAssistantMessage, TurnID: turnID, MessageID: messageID, Text: text, Timestamp: at}
}
func NewActivityEvent(turnID string, kind ActivityKind, summary string) Event {
	return Event{Kind: EventActivity, TurnID: turnID, Activity: kind, Text: summary}
}
func NewBlockedEvent(turnID string, kind BlockedKind) Event {
	return Event{Kind: EventBlocked, TurnID: turnID, Blocked: kind, Text: blockedMessage(kind)}
}
func NewCompletionEvent(turnID string) Event { return Event{Kind: EventCompletion, TurnID: turnID} }
func NewInterruptionEvent(turnID string, reason InterruptionReason) Event {
	return Event{Kind: EventInterruption, TurnID: turnID, Interruption: reason}
}
func NewTerminalFailureEvent(turnID string, failure ProviderError) Event {
	return Event{Kind: EventTerminalFailure, TurnID: turnID, Failure: failure}
}
func (e Event) Validate() error {
	switch e.Kind {
	case EventUserMessage, EventAssistantMessage:
		if !validID(e.TurnID) || !validID(e.MessageID) || !validBoundedText(e.Text, MaxEventTextBytes, true) || e.Timestamp.IsZero() {
			return errors.New("invalid provider message event")
		}
	case EventAssistantDelta:
		if !validID(e.TurnID) || !validID(e.MessageID) || !validBoundedText(e.Text, MaxDeltaBytes, true) {
			return errors.New("invalid provider delta event")
		}
	case EventActivity:
		if (e.TurnID != "" && !validID(e.TurnID)) || !validActivity(e.Activity) || !validBoundedText(e.Text, MaxSummaryBytes, true) {
			return errors.New("invalid provider activity event")
		}
	case EventBlocked:
		if !validID(e.TurnID) {
			return errors.New("invalid provider blocked event turn")
		}
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
	case EventTerminalFailure:
		if (e.TurnID != "" && !validID(e.TurnID)) || !e.Failure.Valid() {
			return errors.New("invalid provider terminal failure event")
		}
	default:
		return errors.New("invalid provider event kind")
	}
	return nil
}
func (Event) MarshalJSON() ([]byte, error) {
	return nil, errors.New("provider events cannot cross the browser wire boundary")
}

func validateProviderAccess(name Name, access AccessMode) error {
	if name != NamePi || access != AccessContentOnly {
		return errors.New("invalid provider access")
	}
	return nil
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
	if !validBoundedText(value, MaxURLBytes, true) {
		return false
	}
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
func validBoundedText(value string, maxBytes int, nonempty bool) bool {
	return utf8.ValidString(value) && len(value) <= maxBytes && (!nonempty || value != "")
}
func validBoundedBytes(value []byte, maxBytes int, nonempty bool) bool {
	return utf8.Valid(value) && len(value) <= maxBytes && (!nonempty || len(value) != 0)
}

var _ error = ProviderError{}
var _ json.Marshaler = NativeSessionRef{}
var _ json.Marshaler = NativeSession{}
var _ json.Marshaler = PageContext{}
var _ json.Marshaler = Event{}
