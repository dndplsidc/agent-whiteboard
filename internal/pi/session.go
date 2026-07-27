//go:build unix

package pi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/provider"
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
	policyBlocked      bool
	providerFailed     bool
	interruptRequested bool
	assistantEmitted   bool
	terminalEmitted    bool
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
	active           *activeTurn
	lastInterrupted  string
	shutdownStarted  bool
	shutdownComplete bool
	shutdownErr      error
	childDone        chan struct{}
	loopsDone        chan struct{}
}

func newSession(driver *Driver, native provider.NativeSession, state startupState, child provider.ManagedChild, client *rpcClient) *Session {
	return &Session{driver: driver, native: native, state: state, child: child, rpc: client, events: make(chan provider.Event, 256), childDone: make(chan struct{}), loopsDone: make(chan struct{})}
}

func (s *Session) start() {
	if errorsStream := s.child.Errors(); errorsStream != nil {
		go func() { _, _ = io.CopyBuffer(io.Discard, errorsStream, make([]byte, 32<<10)) }()
	}
	go func() { _ = s.child.Wait(); close(s.childDone) }()
	go s.eventLoop()
}

func (s *Session) NativeSession() provider.NativeSession { return s.native }
func (s *Session) Model() string                         { return s.state.Model }
func (s *Session) Events() <-chan provider.Event         { return s.events }
func (s *Session) Child() provider.ManagedChild          { return s.child }

func (s *Session) Submit(ctx context.Context, request provider.TurnRequest) (provider.AcceptedTurn, error) {
	if request.Validate() != nil {
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	envelope, err := BuildEnvelope(request)
	if err != nil {
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	turn := &activeTurn{request: request, envelope: envelope, assistantID: assistantMessageID(request.TurnID), rpcEventsAtStart: s.rpc.eventCount(), settled: make(chan struct{})}
	s.mu.Lock()
	if s.active != nil {
		s.mu.Unlock()
		wipe(envelope)
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	s.active = turn
	s.mu.Unlock()
	response, wrote, err := s.rpc.call(ctx, "prompt", map[string]any{"message": string(envelope)})
	if err != nil {
		if wrote {
			return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorAcceptanceUnknown)
		}
		s.clearTurn(turn)
		return provider.AcceptedTurn{}, err
	}
	if requireSuccessfulResponse(response, "prompt") != nil {
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
		return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorAcceptanceUnknown)
	}
	s.mu.Lock()
	if s.active == turn {
		turn.accepted = true
		turn.acceptedAt = acceptedAt
	}
	s.mu.Unlock()
	return provider.AcceptedTurn{TurnID: request.TurnID, AcceptedAt: acceptedAt}, nil
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
	if s.active == turn && !turn.policyBlocked {
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
	if !s.shutdownStarted {
		s.shutdownStarted = true
		s.rpc.inputOnce.Do(func() { _ = s.rpc.input.Close() })
	}
	done := s.loopsDone
	s.mu.Unlock()
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
	h := sha256.New()
	_, _ = h.Write([]byte("agent-whiteboard-pi-assistant-v1\x00"))
	_, _ = h.Write([]byte(turnID))
	sum := h.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(sum[:24])
}

func (s *Session) eventLoop() {
	defer func() {
		s.mu.Lock()
		turn := s.active
		shutdown := s.shutdownStarted
		terminalEmitted := false
		if turn != nil {
			terminalEmitted = turn.terminalEmitted
			turn.settleOnce.Do(func() { close(turn.settled) })
		}
		s.mu.Unlock()
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
