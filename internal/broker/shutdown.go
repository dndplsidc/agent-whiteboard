package broker

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/provider"
)

type actorShutdown struct {
	session       provider.Session
	workerSettled <-chan struct{}
	shutdownDone  chan error
	startOnce     sync.Once
	workerDone    bool
	shutdownEnded bool
	shutdownErr   error
}

func newActorShutdown(session provider.Session, workerSettled <-chan struct{}) *actorShutdown {
	return &actorShutdown{
		session: session, workerSettled: workerSettled,
		shutdownDone: make(chan error, 1), workerDone: workerSettled == nil,
	}
}

func (attempt *actorShutdown) run(ctx context.Context, timeout time.Duration) error {
	if attempt == nil || common.IsNil(attempt.session) || timeout <= 0 {
		return errors.New("invalid actor shutdown")
	}
	gracePeriod := timeout / 2
	if gracePeriod <= 0 {
		gracePeriod = timeout
	}
	graceCtx, cancel := context.WithTimeout(ctx, gracePeriod)
	defer cancel()
	attempt.startOnce.Do(func() {
		go func() {
			attempt.shutdownDone <- attempt.session.Shutdown(graceCtx)
		}()
	})

	for !attempt.workerDone || !attempt.shutdownEnded {
		if attempt.shutdownEnded && attempt.shutdownErr != nil {
			break
		}
		select {
		case <-attempt.workerSettled:
			attempt.workerDone = true
			attempt.workerSettled = nil
		case attempt.shutdownErr = <-attempt.shutdownDone:
			attempt.shutdownEnded = true
			attempt.shutdownDone = nil
		case <-graceCtx.Done():
			goto escalate
		}
	}
	if attempt.workerDone && attempt.shutdownEnded && attempt.shutdownErr == nil {
		return nil
	}

escalate:
	child := attempt.session.Child()
	if common.IsNil(child) {
		return errors.New("provider shutdown failed without a managed child")
	}
	_ = child.Terminate()
	if err := child.Kill(); err != nil {
		return errors.New("provider shutdown escalation failed")
	}
	_ = child.Wait()

	// Child exit must unblock both provider calls before actor ownership ends.
	for !attempt.workerDone || !attempt.shutdownEnded {
		select {
		case <-attempt.workerSettled:
			attempt.workerDone = true
			attempt.workerSettled = nil
		case attempt.shutdownErr = <-attempt.shutdownDone:
			attempt.shutdownEnded = true
			attempt.shutdownDone = nil
		case <-ctx.Done():
			return errors.New("provider shutdown did not join")
		}
	}
	return nil
}
