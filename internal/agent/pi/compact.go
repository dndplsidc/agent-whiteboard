//go:build unix

package pi

import (
	"context"
	"sync"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
)

type activeCompact struct {
	request           provider.CompactRequest
	started           chan struct{}
	startOnce         sync.Once
	terminal          bool
	interrupt         bool
	acceptedAt        time.Time
	startsAtAdmission uint64
}

func (*Session) SupportsCompact() bool { return true }

func (s *Session) Compact(ctx context.Context, request provider.CompactRequest) (provider.AcceptedCompact, error) {
	s.admission.Lock()
	defer s.admission.Unlock()
	if request.Validate() != nil {
		return provider.AcceptedCompact{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	work := &activeCompact{request: request, started: make(chan struct{}), startsAtAdmission: s.rpc.compactionStartCount()}
	s.mu.Lock()
	if s.active != nil || s.compact != nil || s.shutdownStarted {
		s.mu.Unlock()
		return provider.AcceptedCompact{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	s.compact = work
	s.mu.Unlock()
	response, wrote, err := s.rpc.call(ctx, "compact", nil)
	if err != nil {
		if wrote {
			return provider.AcceptedCompact{}, provider.NewProviderError(provider.ErrorAcceptanceUnknown)
		}
		s.clearCompact(work)
		return provider.AcceptedCompact{}, err
	}
	if requireSuccessfulResponse(response, "compact") != nil {
		s.mu.Lock()
		started := !work.acceptedAt.IsZero() || s.rpc.compactionStartCount() > work.startsAtAdmission
		s.mu.Unlock()
		if started {
			return provider.AcceptedCompact{}, provider.NewProviderError(provider.ErrorAcceptanceUnknown)
		}
		s.finishCompact(work, provider.CompactFailed)
		return provider.AcceptedCompact{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	select {
	case <-work.started:
	case <-ctx.Done():
		return provider.AcceptedCompact{}, provider.NewProviderError(provider.ErrorAcceptanceUnknown)
	case <-s.rpc.done:
		return provider.AcceptedCompact{}, provider.NewProviderError(provider.ErrorAcceptanceUnknown)
	}
	s.mu.Lock()
	acceptedAt := work.acceptedAt
	terminal := work.terminal
	s.mu.Unlock()
	if terminal || acceptedAt.IsZero() {
		return provider.AcceptedCompact{}, provider.NewProviderError(provider.ErrorProtocolFailure)
	}
	accepted := provider.AcceptedCompact{WorkID: request.WorkID, AcceptedAt: acceptedAt}
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
		s.mu.Unlock()
		s.emit(provider.NewCompactEvent(work.request.WorkID, provider.CompactFailed))
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
	s.mu.Unlock()
	s.emit(provider.NewCompactEvent(work.request.WorkID, status))
}
func (s *Session) clearCompact(work *activeCompact) {
	s.mu.Lock()
	if s.compact == work {
		s.compact = nil
	}
	s.mu.Unlock()
}

var _ provider.ManualCompactSession = (*Session)(nil)
