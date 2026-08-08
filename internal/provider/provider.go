package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edocsss/agent-whiteboard/internal/agentlimits"
	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/contextdigest"
	"github.com/edocsss/agent-whiteboard/internal/raster"
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
	MaxLaunchItems          = 1024
	MaxLaunchAggregateBytes = 1 << 20
	MaxImagesPerTurn        = agentlimits.MaxImagesPerTurn
	MaxImageBytes           = agentlimits.MaxImageBytes
	MaxTurnImageBytes       = agentlimits.MaxTurnImageBytes
	MaxImageNameBytes       = agentlimits.MaxImageNameBytes
)

type Name string

const (
	NamePi    Name = "pi"
	NameCodex Name = "codex"
)

func (name Name) Valid() bool { return name == NamePi || name == NameCodex }

func AllNames() []Name { return []Name{NamePi, NameCodex} }

type AccessMode string

const (
	AccessContentOnly AccessMode = "content-only"
	AccessConfigured  AccessMode = "configured"
)

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
	ErrorImageInputUnsupported  ProviderErrorCode = "image_input_unsupported"
	ErrorImageUnsupported       ProviderErrorCode = "image_unsupported"
	ErrorImageTooLarge          ProviderErrorCode = "image_too_large"
	ErrorImageTurnLimit         ProviderErrorCode = "image_turn_limit"
	ErrorImageMissing           ProviderErrorCode = "image_missing"
	ErrorImageStorageFailure    ProviderErrorCode = "image_storage_failure"
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
	ErrorImageInputUnsupported:  "The resolved model does not support image input.",
	ErrorImageUnsupported:       "The image input is unsupported or malformed.",
	ErrorImageTooLarge:          "The image input exceeds the per-image limit.",
	ErrorImageTurnLimit:         "The turn exceeds the image input limit.",
	ErrorImageMissing:           "The image input is no longer available.",
	ErrorImageStorageFailure:    "The image input could not be read safely.",
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
		ErrorImageInputUnsupported, ErrorImageUnsupported, ErrorImageTooLarge, ErrorImageTurnLimit,
		ErrorImageMissing, ErrorImageStorageFailure,
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
	if !r.State.Valid() || !r.Provider.Valid() || !validBoundedText(r.Model, MaxTitleBytes, false) || (r.State == Ready && r.Model == "") {
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
	if !s.Ref.Valid() || !s.Provider.Valid() || !validBoundedText(s.Model, MaxTitleBytes, true) || s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() || s.UpdatedAt.Before(s.CreatedAt) {
		return errors.New("invalid native session metadata")
	}
	return nil
}
func (NativeSession) MarshalJSON() ([]byte, error) {
	return nil, errors.New("native session metadata cannot be serialized")
}

type CreateRequest struct {
	Provider  Name
	Access    AccessMode
	Workspace string
}

func (r CreateRequest) Validate() error {
	if validateProviderAccess(r.Provider, r.Access) != nil || !validAbsoluteCleanPath(r.Workspace) {
		return errors.New("invalid provider create request")
	}
	return nil
}

type ResumeRequest struct {
	Provider      Name
	Access        AccessMode
	NativeSession NativeSessionRef
	Workspace     string
}

func (r ResumeRequest) Validate() error {
	if validateProviderAccess(r.Provider, r.Access) != nil || !r.NativeSession.Valid() || !validAbsoluteCleanPath(r.Workspace) {
		return errors.New("invalid provider resume request")
	}
	return nil
}

type InspectRequest struct {
	Provider      Name
	NativeSession NativeSessionRef
}

func (r InspectRequest) Validate() error {
	if !r.Provider.Valid() || !r.NativeSession.Valid() {
		return errors.New("invalid provider inspect request")
	}
	return nil
}

type DeleteRequest struct {
	Provider      Name
	NativeSession NativeSessionRef
}

func (r DeleteRequest) Validate() error {
	if !r.Provider.Valid() || !r.NativeSession.Valid() {
		return errors.New("invalid provider delete request")
	}
	return nil
}

type Driver interface {
	Readiness(context.Context) Readiness
	Create(context.Context, CreateRequest) (Session, error)
	Resume(context.Context, ResumeRequest) (Session, error)
	Inspect(context.Context, InspectRequest) (NativeSession, error)
	// Delete is idempotent: an already-missing native session is success.
	Delete(context.Context, DeleteRequest) error
}

type Session interface {
	// NativeSession returns valid metadata for every successfully created or resumed session.
	NativeSession() NativeSession
	Model() string
	Capabilities() Capabilities
	History(context.Context, HistoryRequest) (HistoryPage, error)
	// Preflight resolves the effective model and verifies complete turn sizing
	// against the current native session before Submit is attempted.
	Preflight(context.Context, PreflightRequest) (PreflightResult, error)
	// Submit returns only after the native provider has accepted the turn. If
	// acceptance cannot be determined, it returns ErrorAcceptanceUnknown.
	Submit(context.Context, TurnRequest) (AcceptedTurn, error)
	// Events returns the stable event channel for this session. It has one
	// broker consumer, and the provider closes it after no further events are
	// possible; callers must not replace or multiplex the channel.
	Events() <-chan Event
	Interrupt(context.Context, AcceptedTurn) error
	// Reconcile determines native acceptance from the durable broker turn ID.
	Reconcile(context.Context, TurnReference) (TurnState, error)
	// Child exposes the dedicated process for broker-owned shutdown escalation.
	Child() ManagedChild
	Shutdown(context.Context) error
}

// LaunchRequest is the complete provider-neutral process specification. The
// provider adapter selects its executable, arguments, environment, and private
// workspace; the launcher must not infer or inherit them.
type LaunchRequest struct {
	Executable       string
	Arguments        []string
	Environment      []string
	WorkingDirectory string
}

func (r LaunchRequest) Validate() error {
	if !validAbsoluteCleanPath(r.Executable) || !validAbsoluteCleanPath(r.WorkingDirectory) || r.Arguments == nil || r.Environment == nil {
		return errors.New("invalid provider launch request")
	}
	if len(r.Arguments) > MaxLaunchItems || len(r.Environment) > MaxLaunchItems {
		return errors.New("provider launch request exceeds item limit")
	}
	total := len(r.Executable) + len(r.WorkingDirectory)
	for _, argument := range r.Arguments {
		if !utf8.ValidString(argument) || strings.ContainsRune(argument, '\x00') {
			return errors.New("invalid provider launch argument")
		}
		total += len(argument)
	}
	seenEnvironment := make(map[string]struct{}, len(r.Environment))
	for _, entry := range r.Environment {
		separator := strings.IndexByte(entry, '=')
		if !utf8.ValidString(entry) || strings.ContainsRune(entry, '\x00') || separator <= 0 {
			return errors.New("invalid provider launch environment")
		}
		name := entry[:separator]
		if _, duplicate := seenEnvironment[name]; duplicate {
			return errors.New("duplicate provider launch environment variable")
		}
		seenEnvironment[name] = struct{}{}
		total += len(entry)
	}
	if total > MaxLaunchAggregateBytes {
		return errors.New("provider launch request exceeds byte limit")
	}
	return nil
}

// Launcher starts one isolated provider process from an explicit specification.
type Launcher interface {
	Launch(context.Context, LaunchRequest) (ManagedChild, error)
}

// ManagedChild owns process I/O and process-group escalation. Provider-native
// graceful shutdown remains Session.Shutdown. Output and Errors are live
// streams whose retained bytes remain readable after Wait. The process-group
// launcher uses a 1 MiB unread ring and one fixed 32 KiB drain buffer, bounding
// total unread output held in memory to 1 MiB + 32 KiB per stream. Callers
// that need all output should drain both streams concurrently to EOF before
// Wait. Wait may force bounded draining; after
// retained bytes are read, overflow is reported by Read without waiting for
// the source to close.
type ManagedChild interface {
	Input() io.WriteCloser
	Output() io.Reader
	Errors() io.Reader
	Wait() error
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
		!validBoundedText(p.Title, MaxTitleBytes, true) || !validURL(p.URL) || !validResource(p.Resource) || p.Digest != contextdigest.Calculate(p.Markdown, p.CreatorContext) {
		return errors.New("invalid page context")
	}
	return nil
}

type TurnRequest struct {
	TurnID    string
	MessageID string
	Message   string
	Images    []ImageInput
	Context   *PageContext
}

func (r TurnRequest) Validate() error {
	if !validID(r.TurnID) || !validID(r.MessageID) || !validBoundedText(r.Message, MaxTurnMessageBytes, false) || (r.Message == "" && len(r.Images) == 0) || validateImageInputs(r.Images) != nil {
		return errors.New("invalid turn request")
	}
	if r.Context != nil {
		return r.Context.Validate()
	}
	return nil
}

// Capabilities describes input modalities of the resolved native model.
type Capabilities struct {
	Images bool
}

func (Capabilities) Validate() error { return nil }

// ImageInput is private provider-neutral metadata for one already validated
// conversation image. Path is never serialized or exposed to the browser.
type ImageInput struct {
	ID        string
	Name      string
	MediaType string
	Bytes     int64
	Path      string
}

// Validate verifies one provider image input independently of a turn.
func (image ImageInput) Validate() error {
	return validateImageInputs([]ImageInput{image})
}

func (ImageInput) MarshalJSON() ([]byte, error) {
	return nil, errors.New("provider image inputs cannot be serialized")
}

func validateImageInputs(images []ImageInput) error {
	if len(images) > MaxImagesPerTurn {
		return errors.New("too many provider image inputs")
	}
	seenIDs := make(map[string]struct{}, len(images))
	seenPaths := make(map[string]struct{}, len(images))
	var total int64
	for _, image := range images {
		if !validID(image.ID) || !validBoundedText(image.Name, MaxImageNameBytes, true) || !validImageMediaType(image.MediaType) || image.Bytes <= 0 || image.Bytes > MaxImageBytes || !validAbsoluteCleanPath(image.Path) {
			return errors.New("invalid provider image input")
		}
		if _, exists := seenIDs[image.ID]; exists {
			return errors.New("duplicate provider image id")
		}
		if _, exists := seenPaths[image.Path]; exists {
			return errors.New("duplicate provider image path")
		}
		seenIDs[image.ID] = struct{}{}
		seenPaths[image.Path] = struct{}{}
		total += image.Bytes
		if total > MaxTurnImageBytes {
			return errors.New("provider image input exceeds aggregate limit")
		}
	}
	return nil
}

func validImageMediaType(mediaType string) bool {
	return raster.SupportedMediaType(mediaType)
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

// PreflightResult reports a conservative provider-effective sizing estimate
// after model resolution. EffectiveCapacityTokens is the usable capacity after
// the reported safety margin.
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

// TurnReference is the durable provider-neutral identity used to reconcile a
// turn after broker restart without requiring an in-memory acceptance time.
type TurnReference struct{ TurnID string }

func (reference TurnReference) Validate() error {
	if !validID(reference.TurnID) {
		return errors.New("invalid turn reference")
	}
	return nil
}

type TurnState string

const (
	// TurnNotAccepted means the provider definitively has no accepted turn for
	// the reference. TurnUnknown means the provider cannot determine the result.
	TurnNotAccepted TurnState = "not_accepted"
	TurnAccepted    TurnState = "accepted"
	TurnRunning     TurnState = "running"
	TurnCompleted   TurnState = "completed"
	TurnInterrupted TurnState = "interrupted"
	TurnUnknown     TurnState = "unknown"
)

func (state TurnState) Valid() bool {
	switch state {
	case TurnNotAccepted, TurnAccepted, TurnRunning, TurnCompleted, TurnInterrupted, TurnUnknown:
		return true
	default:
		return false
	}
}

// Definitive reports whether reconciliation can safely resolve prepared state.
func (state TurnState) Definitive() bool {
	return state.Valid() && state != TurnUnknown
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
	if p.NextCursor != "" && (!validID(p.NextCursor) || len(p.Items) == 0 || p.NextCursor != p.Items[len(p.Items)-1].MessageID) {
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
	EventUserMessage         EventKind = "user_message"
	EventAssistantDelta      EventKind = "assistant_delta"
	EventAssistantMessage    EventKind = "assistant_message"
	EventActivity            EventKind = "activity"
	EventToolActivity        EventKind = "tool_activity"
	EventInteractionRequest  EventKind = "interaction_request"
	EventInteractionResolved EventKind = "interaction_resolved"
	EventBlocked             EventKind = "blocked"
	EventCompletion          EventKind = "completion"
	EventInterruption        EventKind = "interruption"
	EventTerminalFailure     EventKind = "terminal_failure"
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
	Tool         *ToolActivity
	Interaction  *InteractionRequest
	Resolution   *InteractionResolution
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
func NewToolActivityEvent(activity ToolActivity) Event {
	copyOfActivity := activity
	return Event{Kind: EventToolActivity, TurnID: activity.TurnID, Tool: &copyOfActivity}
}
func NewInteractionRequestEvent(request InteractionRequest) Event {
	copyOfRequest := request
	return Event{Kind: EventInteractionRequest, TurnID: request.TurnID, Interaction: &copyOfRequest}
}
func NewInteractionResolvedEvent(resolution InteractionResolution) Event {
	copyOfResolution := resolution
	return Event{Kind: EventInteractionResolved, Resolution: &copyOfResolution}
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
	structured := e.Tool != nil || e.Interaction != nil || e.Resolution != nil
	switch e.Kind {
	case EventUserMessage, EventAssistantMessage:
		if structured || !validID(e.TurnID) || !validID(e.MessageID) || !validBoundedText(e.Text, MaxEventTextBytes, true) || e.Timestamp.IsZero() || e.Activity != "" || e.Blocked != "" || e.Interruption != "" || e.Failure != (ProviderError{}) {
			return errors.New("invalid provider message event")
		}
	case EventAssistantDelta:
		if structured || !validID(e.TurnID) || !validID(e.MessageID) || !validBoundedText(e.Text, MaxDeltaBytes, true) || !e.Timestamp.IsZero() || e.Activity != "" || e.Blocked != "" || e.Interruption != "" || e.Failure != (ProviderError{}) {
			return errors.New("invalid provider delta event")
		}
	case EventActivity:
		if structured || (e.TurnID != "" && !validID(e.TurnID)) || e.MessageID != "" || !validActivity(e.Activity) || !validBoundedText(e.Text, MaxSummaryBytes, true) || !e.Timestamp.IsZero() || e.Blocked != "" || e.Interruption != "" || e.Failure != (ProviderError{}) {
			return errors.New("invalid provider activity event")
		}
	case EventBlocked:
		if structured || !validID(e.TurnID) || e.MessageID != "" || !e.Timestamp.IsZero() || e.Activity != "" || e.Interruption != "" || e.Failure != (ProviderError{}) {
			return errors.New("invalid provider blocked event turn")
		}
		if message := blockedMessage(e.Blocked); message == "" || e.Text != message {
			return errors.New("invalid provider blocked event")
		}
	case EventToolActivity:
		if e.Tool == nil || e.Tool.Validate() != nil || e.TurnID != e.Tool.TurnID || e.Interaction != nil || e.Resolution != nil || e.MessageID != "" || e.Text != "" || !e.Timestamp.IsZero() || e.Activity != "" || e.Blocked != "" || e.Interruption != "" || e.Failure != (ProviderError{}) {
			return errors.New("invalid provider tool activity event")
		}
	case EventInteractionRequest:
		if e.Interaction == nil || e.Interaction.Validate() != nil || e.TurnID != e.Interaction.TurnID || e.Tool != nil || e.Resolution != nil || e.MessageID != "" || e.Text != "" || !e.Timestamp.IsZero() || e.Activity != "" || e.Blocked != "" || e.Interruption != "" || e.Failure != (ProviderError{}) {
			return errors.New("invalid provider interaction request event")
		}
	case EventInteractionResolved:
		if e.Resolution == nil || e.Resolution.Validate() != nil || e.Tool != nil || e.Interaction != nil || e.TurnID != "" || e.MessageID != "" || e.Text != "" || !e.Timestamp.IsZero() || e.Activity != "" || e.Blocked != "" || e.Interruption != "" || e.Failure != (ProviderError{}) {
			return errors.New("invalid provider interaction resolved event")
		}
	case EventCompletion:
		if structured || !validID(e.TurnID) || e.MessageID != "" || e.Text != "" || !e.Timestamp.IsZero() || e.Activity != "" || e.Blocked != "" || e.Interruption != "" || e.Failure != (ProviderError{}) {
			return errors.New("invalid provider completion event")
		}
	case EventInterruption:
		if structured || !validID(e.TurnID) || e.MessageID != "" || e.Text != "" || !e.Timestamp.IsZero() || e.Activity != "" || e.Blocked != "" || !validInterruption(e.Interruption) || e.Failure != (ProviderError{}) {
			return errors.New("invalid provider interruption event")
		}
	case EventTerminalFailure:
		if structured || (e.TurnID != "" && !validID(e.TurnID)) || e.MessageID != "" || e.Text != "" || !e.Timestamp.IsZero() || e.Activity != "" || e.Blocked != "" || e.Interruption != "" || !e.Failure.Valid() {
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
	valid := (name == NamePi || name == NameCodex) && access == AccessConfigured
	if !valid {
		return errors.New("invalid provider access")
	}
	return nil
}
func validID(value string) bool { return common.ValidateID(value) == nil }
func validURL(value string) bool {
	if !validBoundedText(value, MaxURLBytes, true) {
		return false
	}
	return common.ValidPageURL(value)
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
func validAbsoluteCleanPath(value string) bool {
	return validBoundedText(value, MaxNativeReferenceBytes, true) && filepath.IsAbs(value) && filepath.Clean(value) == value
}
func validBoundedText(value string, maxBytes int, nonempty bool) bool {
	return utf8.ValidString(value) && len(value) <= maxBytes && (!nonempty || value != "") && !hasDisallowedC0(value)
}
func validBoundedBytes(value []byte, maxBytes int, nonempty bool) bool {
	return utf8.Valid(value) && len(value) <= maxBytes && (!nonempty || len(value) != 0)
}
func hasDisallowedC0(value string) bool {
	for _, char := range value {
		if char < 0x20 && char != '\t' && char != '\n' && char != '\r' {
			return true
		}
	}
	return false
}

var _ error = ProviderError{}
var _ json.Marshaler = NativeSessionRef{}
var _ json.Marshaler = NativeSession{}
var _ json.Marshaler = PageContext{}
var _ json.Marshaler = Event{}
