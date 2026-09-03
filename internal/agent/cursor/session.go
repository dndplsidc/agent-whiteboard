package cursor

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/acp"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
)

const (
	maxBufferedEvents     = 64
	maxPermissionsPerTurn = 64
	maxCachedToolsPerTurn = 128
)

type sessionPhase uint8

const (
	sessionRunning sessionPhase = iota
	sessionShuttingDown
	sessionFailed
	sessionClosed
)

type turnAdmission uint8

const (
	admissionPending turnAdmission = iota
	admissionAccepted
	admissionRejected
	admissionUnknown
)

type turnPhase uint8

const (
	turnRunning turnPhase = iota
	turnFinalizing
	turnTerminal
)

type terminalCause uint8

const (
	terminalNone terminalCause = iota
	terminalCompleted
	terminalInterrupted
	terminalProtocolFailure
	terminalShutdown
	terminalProviderExit
)

type promptBlockBuilder func(string, []byte, []provider.ImageInput) ([]any, error)

type cachedTool struct {
	title  string
	kind   provider.ToolKind
	status provider.ToolStatus
}

type permissionState uint8

const (
	permissionOpen permissionState = iota
	permissionWriting
	permissionSettled
)

type pendingPermission struct {
	mu             sync.Mutex
	responder      *acp.Responder
	native         map[string]string
	request        provider.InteractionRequest
	optionID       string
	browserClaimed bool
	published      bool
	state          permissionState
	changed        chan struct{}
}

func (p *pendingPermission) signalLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

type promptResult struct {
	delivery acp.Delivery
	err      error
}

type activePrompt struct {
	request                      provider.TurnRequest
	accepted                     chan struct{}
	rejected                     chan struct{}
	done                         chan error // retained for the T3 fixture boundary
	result                       chan promptResult
	promptDone                   chan struct{}
	promptCancel                 context.CancelFunc
	promptCallReturned           bool
	acceptedOnce                 sync.Once
	buffer                       []provider.Event
	assistantID                  string
	assistant                    string
	poisoned                     bool
	terminal                     bool
	closedStreamSeen             bool
	closedStreamRecoveryClaimed  bool
	closedStreamCancellationSent bool
	admissionErr                 error
	cancelState                  permissionState
	admission                    turnAdmission
	phase                        turnPhase
	permissionGateOpen           bool
	tools                        map[string]cachedTool
	toolBytes                    int
	terminalCause                terminalCause
	stopDone                     chan struct{}
	stopErr                      error
}

type replayPhase uint8

const (
	replayOpening replayPhase = iota
	replayComplete
	replayFailed
)

type replayTurn struct {
	turnID    string
	assistant int
	tools     map[string]cachedTool
	toolBytes int
}

type Session struct {
	driver                 *Driver
	rt                     *runtime
	workspace              string
	mu                     sync.Mutex
	native                 provider.NativeSession
	catalog                provider.ModelCatalog
	settings               provider.ExecutionSettings
	presentation           provider.ModelPresentation
	modelConfigID          string
	events                 chan provider.Event
	closed                 bool
	eventsClosed           bool
	phase                  sessionPhase
	shutdownIntent         bool
	cleanupStarted         bool
	active                 *activePrompt
	history                []provider.HistoryItem
	historyBytes           int
	replayPhase            replayPhase
	replayBad              bool // retained for lifecycle fixture observability
	replayAnchor           time.Time
	replaySession          string
	replayUpdates          int
	replayTurn             *replayTurn
	replayIDs              map[string]struct{}
	permissions            map[string]*pendingPermission
	permissionOrder        []string
	permissionOutcomes     map[string]acp.Delivery
	configGeneration       uint64
	promptBlocks           promptBlockBuilder
	shutdownDone           chan struct{}
	shutdownErr            error
	beforePromptReturned   func()
	beforeShutdownStop     func()
	afterShutdownStopClaim func(bool)
}

func newSession(d *Driver, rt *runtime, workspace string) *Session {
	return &Session{driver: d, rt: rt, workspace: workspace, events: make(chan provider.Event, 128), phase: sessionRunning, replayPhase: replayComplete, permissions: map[string]*pendingPermission{}, permissionOutcomes: map[string]acp.Delivery{}, promptBlocks: buildPromptBlocks}
}

// beginReplay opens the replay gate while Session.mu is exclusively owned.
// It must run before the inbound handler is published for session/load.
func (s *Session) beginReplay() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = nil
	s.historyBytes = 0
	s.replayPhase = replayOpening
	s.replayBad = false
	s.replayAnchor = s.driver.config.Clock.Now().UTC()
	s.replaySession = ""
	s.replayUpdates = 0
	s.replayTurn = nil
	s.replayIDs = map[string]struct{}{}
}

func (s *Session) finishOpen(open openResult, loaded bool) error {
	if !bounded(open.SessionID, provider.MaxNativeReferenceBytes, true) {
		return provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	catalog, settings, presentation, err := catalogFromOptions(open.ConfigOptions, s.rt.caps.Image)
	if err != nil {
		return err
	}
	modelConfig, err := modelOption(open.ConfigOptions)
	if err != nil {
		return provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	ref, err := provider.NewNativeSessionRef(open.SessionID)
	if err != nil {
		return provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	now := s.driver.config.Clock.Now().UTC()
	s.mu.Lock()
	if loaded {
		if s.replayBad || s.replayPhase == replayFailed || s.replaySession != "" && s.replaySession != open.SessionID {
			s.replayPhase = replayFailed
			s.mu.Unlock()
			return provider.NewProviderError(provider.ErrorMalformedStream)
		}
		s.replayPhase = replayComplete
	} else {
		s.replayPhase = replayComplete
	}
	s.catalog = catalog
	s.settings = settings
	s.presentation = presentation
	s.modelConfigID = modelConfig.ID
	s.configGeneration++
	s.native = provider.NativeSession{Ref: ref, Provider: provider.NameCursor, Model: settings.Model, Settings: copySettings(settings), Presentation: copyPresentation(presentation), CreatedAt: now, UpdatedAt: now}
	s.mu.Unlock()
	return nil
}

func copySettings(v provider.ExecutionSettings) *provider.ExecutionSettings     { x := v; return &x }
func copyPresentation(v provider.ModelPresentation) *provider.ModelPresentation { x := v; return &x }

func (s *Session) NativeSession() provider.NativeSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.native
	if n.Settings != nil {
		n.Settings = copySettings(*n.Settings)
	}
	if n.Presentation != nil {
		n.Presentation = copyPresentation(*n.Presentation)
	}
	return n
}
func (s *Session) Model() string { s.mu.Lock(); defer s.mu.Unlock(); return s.settings.Model }
func (s *Session) Capabilities() provider.Capabilities {
	return provider.Capabilities{Images: s.rt.caps.Image}
}
func (s *Session) Child() provider.ManagedChild            { return s.rt.child }
func (s *Session) Events() <-chan provider.Event           { return s.events }
func (s *Session) BusyTurnPolicy() provider.BusyTurnPolicy { return provider.BusyTurnPreserveDraft }

func (s *Session) SettingsCatalog(context.Context) (provider.ModelCatalog, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != sessionRunning {
		return provider.ModelCatalog{}, provider.NewProviderError(provider.ErrorNotReady)
	}
	return s.catalog.Clone(), nil
}
func (s *Session) EffectiveSettings(context.Context) (provider.ExecutionSettings, provider.ModelPresentation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != sessionRunning {
		return provider.ExecutionSettings{}, provider.ModelPresentation{}, provider.NewProviderError(provider.ErrorNotReady)
	}
	return s.settings, s.presentation, nil
}

func (s *Session) updateAuthoritativeLocked(options []configOption) (provider.ExecutionSettings, provider.ModelPresentation, string, error) {
	catalog, settings, presentation, err := catalogFromOptions(options, s.rt.caps.Image)
	if err != nil {
		return provider.ExecutionSettings{}, provider.ModelPresentation{}, "", err
	}
	model, err := modelOption(options)
	if err != nil {
		return provider.ExecutionSettings{}, provider.ModelPresentation{}, "", provider.NewProviderError(provider.ErrorProtocolIncompatible)
	}
	s.catalog = catalog
	s.settings = settings
	s.presentation = presentation
	s.modelConfigID = model.ID
	s.native.Model = settings.Model
	s.native.Settings = copySettings(settings)
	s.native.Presentation = copyPresentation(presentation)
	s.native.UpdatedAt = s.driver.config.Clock.Now().UTC()
	s.configGeneration++
	return settings, presentation, model.ID, nil
}

func (s *Session) ApplySettings(ctx context.Context, wanted provider.ExecutionSettings) (provider.ExecutionSettings, provider.ModelPresentation, error) {
	s.mu.Lock()
	if s.phase != sessionRunning || s.active != nil {
		s.mu.Unlock()
		return provider.ExecutionSettings{}, provider.ModelPresentation{}, provider.NewProviderError(provider.ErrorNotReady)
	}
	catalog := s.catalog.Clone()
	sessionID := s.native.Ref.Value()
	configID := s.modelConfigID
	s.mu.Unlock()
	canonical, err := catalog.Canonicalize(wanted)
	if err != nil || canonical != wanted || wanted.Effort != "default" || wanted.Speed != provider.SpeedStandard {
		return provider.ExecutionSettings{}, provider.ModelPresentation{}, provider.NewProviderError(provider.ErrorInvalidModelConfiguration)
	}
	var result struct {
		ConfigOptions []configOption `json:"configOptions"`
	}
	if _, err = s.rt.client.Call(ctx, "session/set_config_option", map[string]any{"sessionId": sessionID, "configId": configID, "value": wanted.Model}, &result); err != nil {
		return provider.ExecutionSettings{}, provider.ModelPresentation{}, classifyRPC(err)
	}
	s.mu.Lock()
	settings, presentation, returnedID, parseErr := s.updateAuthoritativeLocked(result.ConfigOptions)
	s.mu.Unlock()
	if parseErr != nil {
		return provider.ExecutionSettings{}, provider.ModelPresentation{}, provider.NewProviderError(provider.ErrorInvalidModelConfiguration)
	}
	if returnedID != configID || settings != wanted {
		return provider.ExecutionSettings{}, provider.ModelPresentation{}, provider.NewProviderError(provider.ErrorInvalidModelConfiguration)
	}
	return settings, presentation, nil
}

func (s *Session) validatePreflightLocked(request provider.PreflightRequest) (provider.PreflightResult, error) {
	if s.phase != sessionRunning || s.active != nil || !s.native.Ref.Valid() {
		return provider.PreflightResult{}, provider.NewProviderError(provider.ErrorNotReady)
	}
	settings := s.settings
	if request.Turn.Settings != nil {
		if *request.Turn.Settings != s.settings {
			return provider.PreflightResult{}, provider.NewProviderError(provider.ErrorInvalidModelConfiguration)
		}
		settings = *request.Turn.Settings
	}
	model, err := s.catalog.Resolve(settings)
	if err != nil {
		return provider.PreflightResult{}, provider.NewProviderError(provider.ErrorInvalidModelConfiguration)
	}
	if len(request.Turn.Images) > 0 && (!s.rt.caps.Image || !model.SupportsImages) {
		return provider.PreflightResult{}, provider.NewProviderError(provider.ErrorImageInputUnsupported)
	}
	return provider.PreflightResult{CapacityMode: provider.CapacityProviderEnforced, ResolvedModel: settings.Model}, nil
}

func (s *Session) Preflight(_ context.Context, request provider.PreflightRequest) (provider.PreflightResult, error) {
	if request.Validate() != nil {
		return provider.PreflightResult{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	if _, err := provider.Build(request.Turn, provider.PolicyConfigured); err != nil {
		return provider.PreflightResult{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.validatePreflightLocked(request)
}

func (s *Session) Submit(ctx context.Context, request provider.TurnRequest) (provider.AcceptedTurn, error) {
	if request.Validate() != nil {
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	envelope, err := provider.Build(request, provider.PolicyConfigured)
	if err != nil {
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	s.mu.Lock()
	_, preflightErr := s.validatePreflightLocked(provider.PreflightRequest{Turn: request})
	settingsSnapshot := s.settings
	sessionID := s.native.Ref.Value()
	generationSnapshot := s.configGeneration
	builder := s.promptBlocks
	s.mu.Unlock()
	if preflightErr != nil {
		return provider.AcceptedTurn{}, preflightErr
	}
	blocks, err := builder(s.workspace, envelope, request.Images)
	if err != nil {
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorImageMissing)
	}
	promptCtx, promptCancel := context.WithCancel(context.Background())
	turn := &activePrompt{request: request, accepted: make(chan struct{}), rejected: make(chan struct{}), done: make(chan error, 1), result: make(chan promptResult, 1), promptDone: make(chan struct{}), promptCancel: promptCancel, assistantID: safeID("assistant", request.TurnID), admission: admissionPending, phase: turnRunning, permissionGateOpen: true, tools: make(map[string]cachedTool)}
	s.mu.Lock()
	if s.phase != sessionRunning || s.active != nil || s.settings != settingsSnapshot || s.native.Ref.Value() != sessionID || s.configGeneration != generationSnapshot {
		s.mu.Unlock()
		promptCancel()
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorInvalidModelConfiguration)
	}
	s.active = turn
	s.mu.Unlock()
	go func() {
		defer promptCancel()
		var result struct {
			StopReason string `json:"stopReason"`
		}
		delivery, callErr := s.rt.client.Call(promptCtx, "session/prompt", map[string]any{"sessionId": sessionID, "prompt": blocks}, &result)
		s.mu.Lock()
		turn.promptCallReturned = true
		beforePromptReturned := s.beforePromptReturned
		s.mu.Unlock()
		close(turn.promptDone)
		if beforePromptReturned != nil {
			beforePromptReturned()
		}
		s.promptReturned(turn, delivery, result.StopReason, callErr)
		turn.result <- promptResult{delivery: delivery, err: callErr}
	}()
	accepted := func() (provider.AcceptedTurn, error) {
		s.mu.Lock()
		settings := s.settings
		presentation := s.presentation
		s.mu.Unlock()
		return provider.AcceptedTurn{TurnID: request.TurnID, AcceptedAt: s.driver.config.Clock.Now().UTC(), Settings: copySettings(settings), Presentation: copyPresentation(presentation)}, nil
	}
	select {
	case <-turn.accepted:
		return accepted()
	case <-turn.rejected:
		s.mu.Lock()
		admissionErr := turn.admissionErr
		s.mu.Unlock()
		if admissionErr == nil {
			admissionErr = provider.NewProviderError(provider.ErrorMalformedStream)
		}
		return provider.AcceptedTurn{}, admissionErr
	case result := <-turn.result:
		s.mu.Lock()
		wasAccepted := channelClosed(turn.accepted)
		admissionErr := turn.admissionErr
		s.mu.Unlock()
		if wasAccepted {
			return accepted()
		}
		if admissionErr != nil {
			return provider.AcceptedTurn{}, admissionErr
		}
		if result.delivery == acp.NotWritten {
			return provider.AcceptedTurn{}, classifyRPC(result.err)
		}
		var rpcErr *acp.RPCError
		if errors.As(result.err, &rpcErr) {
			return provider.AcceptedTurn{}, classifyRPC(result.err)
		}
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorAcceptanceUnknown)
	case <-ctx.Done():
		s.mu.Lock()
		if turn.admission == admissionPending {
			turn.admission = admissionUnknown
		}
		s.mu.Unlock()
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorAcceptanceUnknown)
	case <-s.rt.client.Done():
		s.mu.Lock()
		if turn.admission == admissionPending {
			turn.admission = admissionUnknown
		}
		s.mu.Unlock()
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorAcceptanceUnknown)
	}
}

func channelClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (s *Session) accept(turn *activePrompt) {
	s.mu.Lock()
	s.acceptLocked(turn)
	s.mu.Unlock()
}

func (s *Session) acceptLocked(turn *activePrompt) {
	if s.active != turn || turn.poisoned || turn.terminal || channelClosed(turn.accepted) {
		return
	}
	user := provider.NewUserMessageEvent(turn.request.TurnID, turn.request.MessageID, turn.request.Content, s.driver.config.Clock.Now().UTC())
	if len(s.events) >= cap(s.events) {
		s.publishLocked(user)
		turn.admissionErr = provider.NewProviderError(provider.ErrorProtocolFailure)
		turn.admission = admissionRejected
		if !channelClosed(turn.rejected) {
			close(turn.rejected)
		}
		return
	}
	turn.acceptedOnce.Do(func() {
		turn.admission = admissionAccepted
		close(turn.accepted)
		s.publishLocked(user)
		for _, event := range turn.buffer {
			s.publishLocked(event)
		}
		turn.buffer = nil
	})
}

func (s *Session) queue(turn *activePrompt, event provider.Event) {
	if event.Validate() != nil {
		s.poisonPrompt(turn)
		return
	}
	s.mu.Lock()
	if s.active != turn || turn.poisoned || turn.terminal {
		s.mu.Unlock()
		return
	}
	if !channelClosed(turn.accepted) {
		if len(turn.buffer) >= maxBufferedEvents {
			s.mu.Unlock()
			s.poisonPrompt(turn)
			return
		}
		turn.buffer = append(turn.buffer, event)
		s.acceptLocked(turn)
		s.mu.Unlock()
		return
	}
	s.publishLocked(event)
	s.mu.Unlock()
}

func validStopReason(reason string) bool {
	if !bounded(reason, 64, false) {
		return false
	}
	switch reason {
	case "end_turn", "max_tokens", "max_turn_requests", "refusal", "cancelled":
		return true
	default:
		return false
	}
}

func (s *Session) observeClosedStreamArtifact(turn *activePrompt) {
	s.mu.Lock()
	if s.active != turn || turn.poisoned || turn.terminal || turn.closedStreamSeen {
		s.mu.Unlock()
		return
	}
	turn.closedStreamSeen = true
	s.acceptLocked(turn)
	if s.active != turn || turn.poisoned || turn.terminal {
		s.mu.Unlock()
		return
	}
	promptDone := turn.promptDone
	grace := s.driver.config.ClosedStreamGrace
	s.mu.Unlock()
	go s.recoverClosedStreamAfterGrace(turn, promptDone, grace)
}

func (s *Session) recoverClosedStreamAfterGrace(turn *activePrompt, promptDone <-chan struct{}, grace time.Duration) {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-promptDone:
		return
	case <-timer.C:
	}

	s.mu.Lock()
	if s.active != turn || turn.poisoned || turn.terminal || !turn.closedStreamSeen || turn.promptCallReturned || s.shutdownIntent {
		s.mu.Unlock()
		return
	}
	if turn.stopDone == nil {
		turn.closedStreamRecoveryClaimed = true
	}
	claim := s.claimStopLocked(turn)
	sessionID := s.native.Ref.Value()
	s.mu.Unlock()

	stopCtx, cancelStop := context.WithTimeout(context.Background(), grace)
	err := s.awaitStop(stopCtx, turn, claim, sessionID)
	cancelStop()
	if err != nil {
		s.poisonPrompt(turn)
		return
	}

	s.mu.Lock()
	if s.active != turn || turn.poisoned || turn.terminal {
		s.mu.Unlock()
		return
	}
	turn.closedStreamCancellationSent = true
	cancelPrompt := turn.promptCancel
	s.mu.Unlock()
	if cancelPrompt == nil {
		s.poisonPrompt(turn)
		return
	}
	cancelPrompt()
}

func closedStreamRecovered(turn *activePrompt, reason string, callErr error) bool {
	if !turn.closedStreamSeen {
		return false
	}
	if callErr == nil {
		return reason == "end_turn" || turn.closedStreamRecoveryClaimed && reason == "cancelled"
	}
	return turn.closedStreamRecoveryClaimed && turn.closedStreamCancellationSent && errors.Is(callErr, context.Canceled)
}

func closedStreamInterrupted(turn *activePrompt, callErr error) bool {
	return turn.closedStreamSeen && !turn.closedStreamRecoveryClaimed && turn.closedStreamCancellationSent && errors.Is(callErr, context.Canceled)
}

func (s *Session) promptReturned(turn *activePrompt, delivery acp.Delivery, reason string, callErr error) {
	s.mu.Lock()
	if s.active != turn || turn.poisoned || turn.terminal {
		s.mu.Unlock()
		return
	}
	turn.permissionGateOpen = false
	turn.phase = turnFinalizing
	s.mu.Unlock()
	settleCtx, cancel := context.WithTimeout(context.Background(), s.driver.config.IdleTimeout)
	settleErr := s.settlePermissions(settleCtx, turn)
	cancel()
	if settleErr != nil {
		s.failTransportAsync()
		return
	}
	if callErr == nil && !validStopReason(reason) {
		s.poisonPrompt(turn)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != turn || turn.poisoned || turn.terminal {
		return
	}
	accepted := channelClosed(turn.accepted)
	if callErr != nil && !accepted {
		if delivery == acp.NotWritten {
			turn.admissionErr = classifyRPC(callErr)
		} else {
			var rpcErr *acp.RPCError
			if errors.As(callErr, &rpcErr) {
				turn.admissionErr = classifyRPC(callErr)
			} else {
				turn.admissionErr = provider.NewProviderError(provider.ErrorAcceptanceUnknown)
			}
		}
		turn.admission = admissionRejected
		if !channelClosed(turn.rejected) {
			close(turn.rejected)
		}
		s.active = nil
		s.clearPermissionRecordsLocked()
		return
	}
	if !accepted {
		s.acceptLocked(turn)
		accepted = channelClosed(turn.accepted)
	}
	if !accepted {
		return
	}
	turn.terminal = true
	turn.phase = turnTerminal
	turn.permissionGateOpen = false
	s.active = nil
	if closedStreamRecovered(turn, reason, callErr) {
		if s.shutdownIntent {
			turn.terminalCause = terminalShutdown
			s.publishLocked(provider.NewInterruptionEvent(turn.request.TurnID, provider.InterruptionShutdown))
		} else {
			turn.terminalCause = terminalCompleted
			if turn.assistant != "" {
				s.publishLocked(provider.NewAssistantMessageEvent(turn.request.TurnID, turn.assistantID, turn.assistant, s.driver.config.Clock.Now().UTC()))
			}
			s.publishLocked(provider.NewCompletionEvent(turn.request.TurnID))
		}
		s.clearPermissionRecordsLocked()
		return
	}
	if closedStreamInterrupted(turn, callErr) {
		turn.terminalCause = terminalInterrupted
		s.publishLocked(provider.NewInterruptionEvent(turn.request.TurnID, provider.InterruptionRequested))
		s.clearPermissionRecordsLocked()
		return
	}
	if callErr != nil {
		turn.terminalCause = terminalProtocolFailure
		s.publishLocked(provider.NewTerminalFailureEvent(turn.request.TurnID, provider.NewProviderError(provider.ErrorProtocolFailure)))
		s.clearPermissionRecordsLocked()
		return
	}
	switch reason {
	case "cancelled":
		turn.terminalCause = terminalInterrupted
		s.publishLocked(provider.NewInterruptionEvent(turn.request.TurnID, provider.InterruptionRequested))
	case "end_turn":
		turn.terminalCause = terminalCompleted
		if turn.assistant != "" {
			s.publishLocked(provider.NewAssistantMessageEvent(turn.request.TurnID, turn.assistantID, turn.assistant, s.driver.config.Clock.Now().UTC()))
		}
		s.publishLocked(provider.NewCompletionEvent(turn.request.TurnID))
	default:
		turn.terminalCause = terminalProtocolFailure
		s.publishLocked(provider.NewTerminalFailureEvent(turn.request.TurnID, provider.NewProviderError(provider.ErrorProtocolFailure)))
	}
	s.clearPermissionRecordsLocked()
}

func (s *Session) finishPrompt(turn *activePrompt, reason string) {
	s.promptReturned(turn, acp.Complete, reason, nil)
}
func (s *Session) failPrompt(turn *activePrompt, _ error) { s.poisonPrompt(turn) }

func (s *Session) poisonPrompt(turn *activePrompt) { s.poisonPromptWithInbound(turn, true) }

func (s *Session) poisonPromptWithInbound(turn *activePrompt, inboundComplete bool) {
	s.mu.Lock()
	if turn == nil || turn.poisoned || turn.terminal {
		s.mu.Unlock()
		return
	}
	turn.poisoned = true
	turn.phase = turnFinalizing
	turn.permissionGateOpen = false
	turn.buffer = nil
	turn.assistant = ""
	turn.admissionErr = provider.NewProviderError(provider.ErrorMalformedStream)
	accepted := channelClosed(turn.accepted)
	if !accepted && turn.rejected != nil && !channelClosed(turn.rejected) {
		turn.admission = admissionRejected
		close(turn.rejected)
	}
	s.mu.Unlock()
	settleCtx, cancel := context.WithTimeout(context.Background(), s.driver.config.IdleTimeout)
	settleErr := s.settlePermissions(settleCtx, turn)
	cancel()
	s.mu.Lock()
	if settleErr == nil && inboundComplete && accepted && !turn.terminal {
		turn.terminal = true
		turn.phase = turnTerminal
		turn.terminalCause = terminalProtocolFailure
		if s.active == turn {
			s.active = nil
		}
		s.publishLocked(provider.NewTerminalFailureEvent(turn.request.TurnID, provider.NewProviderError(provider.ErrorProtocolFailure)))
		s.clearPermissionRecordsLocked()
	} else if !accepted && s.active == turn {
		s.active = nil
		if settleErr == nil && inboundComplete {
			s.clearPermissionRecordsLocked()
		}
	}
	s.mu.Unlock()
	s.failTransportAsync()
}
func (s *Session) clearPermissionRecordsLocked() {
	s.permissions = make(map[string]*pendingPermission)
	s.permissionOrder = nil
	s.permissionOutcomes = make(map[string]acp.Delivery)
}

func (s *Session) failTransportAsync() {
	s.mu.Lock()
	if s.cleanupStarted {
		s.mu.Unlock()
		return
	}
	s.cleanupStarted = true
	s.phase = sessionFailed
	if turn := s.active; turn != nil {
		turn.permissionGateOpen = false
	}
	s.mu.Unlock()
	go func() { _ = s.rt.close(context.Background()) }()
}

func (s *Session) publishLocked(event provider.Event) bool {
	if s.closed || s.eventsClosed || event.Validate() != nil {
		return false
	}
	select {
	case s.events <- event:
		return true
	default:
		failedTurn := s.active
		if turn := failedTurn; turn != nil {
			turn.poisoned = true
			turn.terminal = true
			turn.phase = turnTerminal
			turn.terminalCause = terminalProtocolFailure
			turn.permissionGateOpen = false
			s.active = nil
		}
		s.phase = sessionFailed
		s.closed = true
		s.closeEventsLocked()
		if !s.cleanupStarted {
			s.cleanupStarted = true
			go func(turn *activePrompt) {
				ctx, cancel := context.WithTimeout(context.Background(), s.driver.config.IdleTimeout)
				if turn != nil {
					_ = s.settlePermissions(ctx, turn)
				}
				cancel()
				_ = s.rt.close(context.Background())
			}(failedTurn)
		}
		return false
	}
}
func (s *Session) emit(event provider.Event) {
	s.mu.Lock()
	s.publishLocked(event)
	s.mu.Unlock()
}

type stopClaim struct {
	done  chan struct{}
	owner bool
}

func (s *Session) claimStopLocked(turn *activePrompt) stopClaim {
	turn.permissionGateOpen = false
	if turn.stopDone != nil {
		return stopClaim{done: turn.stopDone}
	}
	turn.stopDone = make(chan struct{})
	turn.cancelState = permissionWriting
	return stopClaim{done: turn.stopDone, owner: true}
}

func (s *Session) awaitStop(ctx context.Context, turn *activePrompt, claim stopClaim, sessionID string) error {
	if !claim.owner {
		select {
		case <-claim.done:
			s.mu.Lock()
			err := turn.stopErr
			s.mu.Unlock()
			return err
		case <-ctx.Done():
			return provider.NewProviderError(provider.ErrorProtocolFailure)
		}
	}
	err := s.performStop(ctx, turn, sessionID)
	s.mu.Lock()
	turn.stopErr = err
	close(claim.done)
	s.mu.Unlock()
	return err
}

func (s *Session) Interrupt(ctx context.Context, accepted provider.AcceptedTurn) error {
	if accepted.Validate() != nil {
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	s.mu.Lock()
	turn := s.active
	if turn == nil || turn.request.TurnID != accepted.TurnID {
		s.mu.Unlock()
		return nil
	}
	claim := s.claimStopLocked(turn)
	sessionID := s.native.Ref.Value()
	s.mu.Unlock()
	err := s.awaitStop(ctx, turn, claim, sessionID)
	if err != nil {
		s.failTransportAsync()
	}
	return err
}

func (s *Session) performStop(ctx context.Context, turn *activePrompt, sessionID string) error {
	if err := s.settlePermissions(ctx, turn); err != nil {
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	delivery, err := s.rt.client.Notify(ctx, "session/cancel", map[string]any{"sessionId": sessionID})
	s.mu.Lock()
	turn.cancelState = permissionSettled
	s.mu.Unlock()
	if err != nil || delivery != acp.Complete {
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	return nil
}

func (s *Session) resolveTransportPermissionsLocked() bool {
	allComplete := true
	for _, delivery := range s.permissionOutcomes {
		if delivery != acp.Complete {
			allComplete = false
		}
	}
	for _, id := range s.permissionOrder {
		pending := s.permissions[id]
		if pending == nil {
			continue
		}
		outcome := pending.responder.Outcome()
		delivery := outcome.Delivery
		if !outcome.Settled {
			delivery = acp.NotWritten
		}
		s.recordPermissionOutcomeLocked(id, pending, delivery)
		if delivery != acp.Complete {
			allComplete = false
		}
	}
	return allComplete
}

func (s *Session) transportEnded() {
	s.mu.Lock()
	permissionsComplete := s.resolveTransportPermissionsLocked()
	if s.closed {
		s.clearPermissionRecordsLocked()
		s.closeEventsLocked()
		s.mu.Unlock()
		return
	}
	turn := s.active
	if permissionsComplete && s.phase != sessionFailed && turn != nil && channelClosed(turn.accepted) && !turn.terminal {
		turn.terminal = true
		turn.terminalCause = terminalProviderExit
		cause := provider.InterruptionProviderExit
		if s.shutdownIntent {
			turn.terminalCause = terminalShutdown
			cause = provider.InterruptionShutdown
		}
		s.publishLocked(provider.NewInterruptionEvent(turn.request.TurnID, cause))
	}
	s.closed = true
	s.phase = sessionClosed
	s.active = nil
	s.clearPermissionRecordsLocked()
	s.closeEventsLocked()
	s.mu.Unlock()
}

func (s *Session) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.phase == sessionClosed {
		err := s.shutdownErr
		s.mu.Unlock()
		return err
	}
	if s.shutdownDone != nil {
		done := s.shutdownDone
		s.mu.Unlock()
		select {
		case <-done:
			s.mu.Lock()
			err := s.shutdownErr
			s.mu.Unlock()
			return err
		case <-ctx.Done():
			return provider.NewProviderError(provider.ErrorProtocolFailure)
		}
	}
	s.shutdownDone = make(chan struct{})
	shutdownDone := s.shutdownDone
	s.shutdownIntent = true
	s.phase = sessionShuttingDown
	turn := s.active
	sessionID := s.native.Ref.Value()
	if turn != nil {
		turn.permissionGateOpen = false
	}
	beforeStop := s.beforeShutdownStop
	s.mu.Unlock()
	if beforeStop != nil {
		beforeStop()
	}
	var stopErr error
	if turn != nil {
		s.mu.Lock()
		claim := s.claimStopLocked(turn)
		afterClaim := s.afterShutdownStopClaim
		s.mu.Unlock()
		if afterClaim != nil {
			afterClaim(claim.owner)
		}
		stopErr = s.awaitStop(ctx, turn, claim, sessionID)
	} else {
		stopErr = s.cancelPermissions(ctx)
	}
	closeErr := s.rt.close(ctx)
	s.mu.Lock()
	permissionsComplete := s.resolveTransportPermissionsLocked()
	if stopErr == nil && permissionsComplete && turn != nil && channelClosed(turn.accepted) && !turn.terminal {
		turn.terminal = true
		turn.phase = turnTerminal
		turn.terminalCause = terminalShutdown
		s.publishLocked(provider.NewInterruptionEvent(turn.request.TurnID, provider.InterruptionShutdown))
	}
	s.closed = true
	s.phase = sessionClosed
	s.active = nil
	s.clearPermissionRecordsLocked()
	s.closeEventsLocked()
	s.shutdownErr = errors.Join(stopErr, closeErr)
	close(shutdownDone)
	err := s.shutdownErr
	s.mu.Unlock()
	return err
}
func (s *Session) closeEventsLocked() {
	if !s.eventsClosed {
		s.eventsClosed = true
		close(s.events)
	}
}

func (s *Session) History(_ context.Context, request provider.HistoryRequest) (provider.HistoryPage, error) {
	if request.Validate() != nil {
		return provider.HistoryPage{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.replayPhase != replayComplete {
		return provider.HistoryPage{}, provider.NewProviderError(provider.ErrorMalformedStream)
	}
	items := append([]provider.HistoryItem(nil), s.history...)
	for index := range items {
		items[index].Content = items[index].Content.Clone()
	}
	end := len(items)
	if request.BeforeMessageID != "" {
		end = -1
		for i := range items {
			if items[i].MessageID == request.BeforeMessageID {
				end = i
				break
			}
		}
		if end < 0 {
			return provider.HistoryPage{}, provider.NewProviderError(provider.ErrorProtocolFailure)
		}
	}
	limit := request.Limit
	if limit == 0 {
		limit = provider.MaxHistoryItems
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	page := provider.HistoryPage{Items: make([]provider.HistoryItem, 0, end-start)}
	for index := end - 1; index >= start; index-- {
		page.Items = append(page.Items, items[index])
	}
	if start > 0 && len(page.Items) > 0 {
		page.NextCursor = page.Items[len(page.Items)-1].MessageID
	}
	if page.Validate() != nil {
		return provider.HistoryPage{}, provider.NewProviderError(provider.ErrorMalformedStream)
	}
	return page, nil
}
func (s *Session) Reconcile(_ context.Context, ref provider.TurnReference) (provider.TurnState, error) {
	if ref.Validate() != nil {
		return provider.TurnUnknown, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.replayPhase != replayComplete {
		return provider.TurnUnknown, provider.NewProviderError(provider.ErrorMalformedStream)
	}
	count := 0
	for _, item := range s.history {
		if item.Role == provider.HistoryUser && item.TurnID == ref.TurnID {
			count++
		}
	}
	if count == 1 {
		return provider.TurnAccepted, nil
	}
	if count == 0 {
		return provider.TurnNotAccepted, nil
	}
	return provider.TurnUnknown, provider.NewProviderError(provider.ErrorMalformedStream)
}
func safeID(prefix, value string) string {
	sum := sha256.Sum256([]byte(prefix + "\x00" + value))
	return base64.RawURLEncoding.EncodeToString(sum[:24])
}

var _ provider.Session = (*Session)(nil)
var _ provider.SettingsSession = (*Session)(nil)
var _ provider.InteractiveSession = (*Session)(nil)
var _ provider.BusyTurnSession = (*Session)(nil)
