//go:build unix

package pi

import (
	"context"
	"sync"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
)

type activeCompact struct {
	request    provider.CompactRequest
	started    chan struct{}
	startOnce  sync.Once
	callDone   chan struct{}
	callResult compactCallResult
	terminal   bool
	interrupt  bool
	acceptedAt time.Time
}

type compactCallResult struct {
	response rpcResponse
	wrote    bool
	err      error
}

func (*Session) SupportsCompact() bool { return true }

func (s *Session) Compact(ctx context.Context, request provider.CompactRequest) (provider.AcceptedCompact, error) {
	s.admission.Lock()
	defer s.admission.Unlock()
	if request.Validate() != nil {
		return provider.AcceptedCompact{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	work := &activeCompact{request: request, started: make(chan struct{}), callDone: make(chan struct{})}
	s.mu.Lock()
	if s.active != nil || s.compact != nil || s.shutdownStarted {
		s.mu.Unlock()
		return provider.AcceptedCompact{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	s.compact = work
	s.mu.Unlock()

	// Pi resolves the compact RPC only after its compaction stream has ended.
	// Keep that call session-owned so caller cancellation cannot abandon its
	// response, while compaction_start can establish acceptance immediately.
	go s.runCompactCall(work)

	callDone := work.callDone
	for {
		select {
		case <-work.started:
			return s.acceptCompact(work)
		case <-callDone:
			callDone = nil
			s.mu.Lock()
			result := work.callResult
			accepted := !work.acceptedAt.IsZero()
			s.mu.Unlock()
			if accepted {
				return s.acceptCompact(work)
			}
			if result.err != nil {
				if result.wrote {
					return provider.AcceptedCompact{}, provider.NewProviderError(provider.ErrorAcceptanceUnknown)
				}
				return provider.AcceptedCompact{}, result.err
			}
			if requireSuccessfulResponse(result.response, "compact") != nil {
				return provider.AcceptedCompact{}, provider.NewProviderError(provider.ErrorProtocolFailure)
			}
		case <-ctx.Done():
			return provider.AcceptedCompact{}, provider.NewProviderError(provider.ErrorAcceptanceUnknown)
		case <-s.rpc.done:
			s.mu.Lock()
			accepted := !work.acceptedAt.IsZero()
			s.mu.Unlock()
			if accepted {
				return s.acceptCompact(work)
			}
			return provider.AcceptedCompact{}, provider.NewProviderError(provider.ErrorAcceptanceUnknown)
		}
	}
}

func (s *Session) runCompactCall(work *activeCompact) {
	response, wrote, err := s.rpc.call(context.Background(), "compact", nil)
	result := compactCallResult{response: response, wrote: wrote, err: err}
	failed := err != nil || requireSuccessfulResponse(response, "compact") != nil

	s.mu.Lock()
	work.callResult = result
	accepted := !work.acceptedAt.IsZero()
	terminal := work.terminal
	owned := s.compact == work
	s.mu.Unlock()

	if failed && owned && !terminal {
		if accepted || err == nil {
			s.finishCompact(work, provider.CompactFailed)
		} else if !wrote {
			s.clearCompact(work)
		}
	}
	close(work.callDone)
}

func (s *Session) acceptCompact(work *activeCompact) (provider.AcceptedCompact, error) {
	s.mu.Lock()
	acceptedAt := work.acceptedAt
	s.mu.Unlock()
	if acceptedAt.IsZero() {
		return provider.AcceptedCompact{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	accepted := provider.AcceptedCompact{WorkID: work.request.WorkID, AcceptedAt: acceptedAt}
	if accepted.Validate() != nil {
		return provider.AcceptedCompact{}, provider.NewProviderError(provider.ErrorAcceptanceUnknown)
	}
	return accepted, nil
}

func (s *Session) InterruptCompact(ctx context.Context, accepted provider.AcceptedCompact) error {
	if accepted.Validate() != nil {
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	s.mu.Lock()
	work := s.compact
	if work == nil || work.request.WorkID != accepted.WorkID || !work.acceptedAt.Equal(accepted.AcceptedAt) || work.terminal {
		s.mu.Unlock()
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	if work.interrupt {
		s.mu.Unlock()
		return nil
	}
	work.interrupt = true
	s.mu.Unlock()
	response, wrote, err := s.rpc.call(ctx, "abort", nil)
	if err != nil {
		if !wrote {
			s.mu.Lock()
			if s.compact == work {
				work.interrupt = false
			}
			s.mu.Unlock()
		}
		return err
	}
	if requireSuccessfulResponse(response, "abort") != nil {
		s.mu.Lock()
		if s.compact == work {
			work.interrupt = false
		}
		s.mu.Unlock()
		return provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	return nil
}

func (s *Session) compactStarted() bool {
	s.mu.Lock()
	work := s.compact
	if work == nil || work.terminal {
		s.mu.Unlock()
		return false
	}
	if work.acceptedAt.IsZero() {
		work.acceptedAt = s.driver.config.Clock.Now().UTC()
	}
	work.startOnce.Do(func() { close(work.started) })
	s.mu.Unlock()
	return true
}

func (s *Session) compactEnded() bool {
	s.mu.Lock()
	work := s.compact
	if work == nil || work.terminal {
		s.mu.Unlock()
		return false
	}
	if work.acceptedAt.IsZero() {
		work.terminal = true
		s.compact = nil
		work.startOnce.Do(func() { close(work.started) })
		s.emit(provider.NewCompactEvent(work.request.WorkID, provider.CompactFailed))
		s.mu.Unlock()
		return true
	}
	interrupted := work.interrupt
	s.mu.Unlock()
	status := provider.CompactCompleted
	if interrupted {
		status = provider.CompactInterrupted
	}
	s.finishCompact(work, status)
	return true
}

func (s *Session) finishCompact(work *activeCompact, status provider.CompactStatus) {
	s.mu.Lock()
	if s.compact != work || work.terminal {
		s.mu.Unlock()
		return
	}
	work.terminal = true
	s.compact = nil
	s.emit(provider.NewCompactEvent(work.request.WorkID, status))
	s.mu.Unlock()
}
func (s *Session) clearCompact(work *activeCompact) {
	s.mu.Lock()
	if s.compact == work {
		s.compact = nil
	}
	s.mu.Unlock()
}

var _ provider.ManualCompactSession = (*Session)(nil)
