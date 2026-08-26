//go:build unix

package pi

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
)

type activeTurn struct {
	request            provider.TurnRequest
	envelope           []byte
	assistantID        string
	accepted           bool
	acceptedAt         time.Time
	rpcEventsAtStart   uint64
	nativeSeen         bool
	userEmitted        bool
	abortSent          bool
	providerFailed     bool
	interruptRequested bool
	assistantEmitted   bool
	terminalEmitted    bool
	skill              *piSkill
	skillValidated     chan struct{}
	skillValidateOnce  sync.Once
	settled            chan struct{}
	settleOnce         sync.Once
}

type Session struct {
	driver *Driver
	native provider.NativeSession
	state  startupState
	child  provider.ManagedChild
	rpc    *rpcClient
	events chan provider.Event

	mu               sync.Mutex
	admission        sync.Mutex
	catalog          provider.ModelCatalog
	models           map[string]piModel
	skills           piSkillCatalog
	skillsCatalog    provider.SkillCatalog
	interactions     map[string]piInteraction
	lastNotification string
	active           *activeTurn
	compact          *activeCompact
	lastInterrupted  string
	shutdownStarted  bool
	shutdownComplete bool
	shutdownErr      error
	childDone        chan struct{}
	loopsDone        chan struct{}
}

func newSession(driver *Driver, native provider.NativeSession, state startupState, child provider.ManagedChild, client *rpcClient) *Session {
	return &Session{driver: driver, native: native, state: state, child: child, rpc: client, events: make(chan provider.Event, 256), models: make(map[string]piModel), skills: piSkillCatalog{byID: make(map[string]piSkill), order: []string{}}, skillsCatalog: unavailablePiSkills(), interactions: make(map[string]piInteraction), childDone: make(chan struct{}), loopsDone: make(chan struct{})}
}

func (s *Session) start() {
	if errorsStream := s.child.Errors(); errorsStream != nil {
		go func() { _, _ = io.CopyBuffer(io.Discard, errorsStream, make([]byte, 32<<10)) }()
	}
	go func() { _ = s.child.Wait(); close(s.childDone) }()
	go s.eventLoop()
}

func (s *Session) NativeSession() provider.NativeSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	native := s.native
	if native.Settings != nil {
		copyOf := *native.Settings
		native.Settings = &copyOf
	}
	if native.Presentation != nil {
		copyOf := *native.Presentation
		native.Presentation = &copyOf
	}
	return native
}
func (s *Session) Model() string { s.mu.Lock(); defer s.mu.Unlock(); return s.state.Model }
func (s *Session) Capabilities() provider.Capabilities {
	s.mu.Lock()
	defer s.mu.Unlock()
	return provider.Capabilities{Images: s.state.SupportsImages}
}
func (s *Session) Events() <-chan provider.Event { return s.events }
func (s *Session) Child() provider.ManagedChild  { return s.child }

func (s *Session) Submit(ctx context.Context, request provider.TurnRequest) (provider.AcceptedTurn, error) {
	s.admission.Lock()
	defer s.admission.Unlock()
	if request.Validate() != nil {
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	s.mu.Lock()
	unavailable := s.active != nil || s.compact != nil || s.shutdownStarted
	s.mu.Unlock()
	if unavailable {
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	hasSkillPart := false
	for _, part := range request.Content.Parts {
		hasSkillPart = hasSkillPart || part.Kind == provider.MessagePartSkill
	}
	if hasSkillPart {
		if _, err := s.refreshSkills(ctx); err != nil {
			return provider.AcceptedTurn{}, err
		}
	}
	skill, hasSkill, err := s.selectedSkill(request.Content)
	if err != nil {
		return provider.AcceptedTurn{}, err
	}
	envelope, err := BuildEnvelope(request)
	if err != nil {
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	var prepared *preparedPiSettings
	if request.Settings != nil {
		value, prepareErr := s.prepareSettings(ctx, *request.Settings)
		if prepareErr != nil {
			wipe(envelope)
			return provider.AcceptedTurn{}, prepareErr
		}
		prepared = &value
		if len(request.Images) != 0 && !value.model.images {
			wipe(envelope)
			return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorImageInputUnsupported)
		}
	} else {
		s.mu.Lock()
		supportsImages := s.state.SupportsImages
		s.mu.Unlock()
		if len(request.Images) != 0 && !supportsImages {
			wipe(envelope)
			return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorImageInputUnsupported)
		}
	}
	fields, err := buildPromptFields(envelope, request.Images)
	if err != nil {
		wipe(envelope)
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorImageStorageFailure)
	}
	s.mu.Lock()
	before, _ := s.state.settings()
	s.mu.Unlock()
	var acceptedSettings *provider.ExecutionSettings
	var acceptedPresentation *provider.ModelPresentation
	settingsChanged := false
	if prepared != nil {
		effective, presentation, applyErr := s.applySettingsAdmitted(ctx, *prepared)
		if applyErr != nil {
			wipe(envelope)
			return provider.AcceptedTurn{}, applyErr
		}
		acceptedSettings, acceptedPresentation = &effective, &presentation
		settingsChanged = effective != before
	}
	publishSettings := func() {
		if settingsChanged {
			s.emit(provider.NewVerifiedSettingsEvent("", *acceptedSettings, *acceptedPresentation))
		}
	}
	turn := &activeTurn{request: request, envelope: envelope, assistantID: assistantMessageID(request.TurnID), rpcEventsAtStart: s.rpc.eventCount(), skillValidated: make(chan struct{}), settled: make(chan struct{})}
	s.mu.Lock()
	if s.active != nil || s.compact != nil || s.shutdownStarted {
		s.mu.Unlock()
		publishSettings()
		wipe(envelope)
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	s.active = turn
	s.mu.Unlock()
	if hasSkill {
		fields["message"] = piSkillPrompt(skill, envelope)
		turn.skill = &skill
	}
	response, wrote, err := s.rpc.call(ctx, "prompt", fields)
	if err != nil {
		publishSettings()
		if wrote {
			return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorAcceptanceUnknown)
		}
		s.clearTurn(turn)
		return provider.AcceptedTurn{}, err
	}
	if requireSuccessfulResponse(response, "prompt") != nil {
		publishSettings()
		s.mu.Lock()
		nativeEvents := turn.nativeSeen || s.rpc.eventCount() > turn.rpcEventsAtStart
		s.mu.Unlock()
		if nativeEvents {
			return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorAcceptanceUnknown)
		}
		s.clearTurn(turn)
		return provider.AcceptedTurn{}, classifyPromptRejection(response)
	}
	acceptedAt := s.driver.config.Clock.Now().UTC()
	if acceptedAt.IsZero() {
		publishSettings()
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorAcceptanceUnknown)
	}
	s.mu.Lock()
	if s.active == turn {
		turn.accepted = true
		turn.acceptedAt = acceptedAt
	}
	s.mu.Unlock()
	if hasSkill {
		select {
		case <-turn.skillValidated:
		case <-ctx.Done():
			publishSettings()
			return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorAcceptanceUnknown)
		case <-s.rpc.done:
			publishSettings()
			return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorAcceptanceUnknown)
		}
	}
	return provider.AcceptedTurn{TurnID: request.TurnID, AcceptedAt: acceptedAt, Settings: acceptedSettings, Presentation: acceptedPresentation}, nil
}

func (s *Session) Interrupt(ctx context.Context, accepted provider.AcceptedTurn) error {
	if accepted.Validate() != nil {
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.lastInterrupted == accepted.TurnID {
		s.mu.Unlock()
		return nil
	}
	turn := s.active
	if turn == nil || turn.request.TurnID != accepted.TurnID || !turn.accepted || !turn.acceptedAt.Equal(accepted.AcceptedAt) {
		s.mu.Unlock()
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	if turn.interruptRequested {
		settled := turn.settled
		s.mu.Unlock()
		return waitContext(ctx, settled)
	}
	turn.interruptRequested = true
	already := turn.abortSent
	turn.abortSent = true
	settled := turn.settled
	s.mu.Unlock()
	if !already {
		response, wrote, err := s.rpc.call(ctx, "abort", nil)
		if err != nil {
			if wrote {
				return provider.NewProviderError(provider.ErrorAcceptanceUnknown)
			}
			s.rollbackInterrupt(turn)
			return err
		}
		if requireSuccessfulResponse(response, "abort") != nil {
			s.rollbackInterrupt(turn)
			return provider.NewProviderError(provider.ErrorProtocolFailure)
		}
	}
	return waitContext(ctx, settled)
}

func (s *Session) rollbackInterrupt(turn *activeTurn) {
	s.mu.Lock()
	if s.active == turn {
		turn.interruptRequested = false
		turn.abortSent = false
	}
	s.mu.Unlock()
}

func waitContext(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Session) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.shutdownComplete {
		err := s.shutdownErr
		s.mu.Unlock()
		return err
	}
	initiated := false
	var compact *activeCompact
	var interactions []piInteraction
	if !s.shutdownStarted {
		s.shutdownStarted = true
		initiated = true
		compact = s.compact
		interactions = s.claimShutdownInteractionsLocked()
	}
	done := s.loopsDone
	s.mu.Unlock()
	if initiated {
		for _, interaction := range interactions {
			_ = s.rpc.notify(ctx, map[string]any{"type": "extension_ui_response", "id": interaction.nativeID, "cancelled": true})
		}
		s.rpc.inputOnce.Do(func() { _ = s.rpc.input.Close() })
		if compact != nil {
			s.finishCompact(compact, provider.CompactInterrupted)
		}
	}
	select {
	case <-done:
		s.mu.Lock()
		s.shutdownComplete = true
		s.shutdownErr = nil
		s.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Session) clearTurn(turn *activeTurn) {
	s.mu.Lock()
	if s.active == turn {
		s.active = nil
		wipe(turn.envelope)
	}
	s.mu.Unlock()
}

func (s *Session) finishTurn(turn *activeTurn, interrupted bool) {
	s.mu.Lock()
	if s.active == turn {
		if interrupted {
			s.lastInterrupted = turn.request.TurnID
		}
		turn.settleOnce.Do(func() { close(turn.settled) })
		s.active = nil
		wipe(turn.envelope)
	}
	s.mu.Unlock()
	s.resolveInteractions()
}

func classifyPromptRejection(response rpcResponse) error {
	var signal struct {
		Code      string `json:"code"`
		ErrorCode string `json:"errorCode"`
	}
	if len(response.Data) != 0 && json.Unmarshal(response.Data, &signal) == nil {
		code := signal.Code
		if code == "" {
			code = signal.ErrorCode
		}
		switch code {
		case "authentication_required":
			return provider.NewProviderError(provider.ErrorAuthenticationRequired)
		case "context_too_large":
			return provider.NewProviderError(provider.ErrorContextTooLarge)
		}
	}
	return provider.NewProviderError(provider.ErrorProtocolFailure)
}

func assistantMessageID(turnID string) string {
	return provider.AssistantMessageID(turnID)
}

func (s *Session) eventLoop() {
	defer func() {
		s.mu.Lock()
		turn := s.active
		compact := s.compact
		shutdown := s.shutdownStarted
		terminalEmitted := false
		if turn != nil {
			terminalEmitted = turn.terminalEmitted
			turn.settleOnce.Do(func() { close(turn.settled) })
		}
		s.mu.Unlock()
		if compact != nil {
			s.finishCompact(compact, provider.CompactFailed)
		}
		s.resolveInteractions()
		if turn != nil && !terminalEmitted {
			if shutdown {
				s.emit(provider.NewInterruptionEvent(turn.request.TurnID, provider.InterruptionShutdown))
			} else {
				failure := provider.NewProviderError(provider.ErrorChildExited)
				if typed, ok := s.rpc.terminalError().(provider.ProviderError); ok && typed.Valid() {
					failure = typed
				}
				s.emit(provider.NewTerminalFailureEvent(turn.request.TurnID, failure))
			}
		}
		close(s.events)
		<-s.childDone
		if s.driver != nil {
			s.driver.release(s.native.Ref.Value(), s)
		}
		close(s.loopsDone)
	}()
	for raw := range s.rpc.events {
		s.handleNativeEvent(raw)
		wipe(raw)
	}
}

func (s *Session) emit(event provider.Event) {
	if event.Validate() != nil {
		return
	}
	select {
	case s.events <- event:
	default:
		s.rpc.finish(provider.NewProviderError(provider.ErrorMalformedStream))
	}
}

var _ provider.Session = (*Session)(nil)
var _ provider.SettingsSession = (*Session)(nil)
var _ provider.SkillCatalogSession = (*Session)(nil)
var _ provider.BusyTurnSession = (*Session)(nil)
var _ provider.ManualCompactSession = (*Session)(nil)
var _ provider.InteractiveSession = (*Session)(nil)
