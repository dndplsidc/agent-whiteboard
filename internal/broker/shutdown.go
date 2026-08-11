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
	session          provider.Session
	child            provider.ManagedChild
	workerSettled    <-chan struct{}
	shutdownDone     chan error
	childWaitDone    chan error
	startOnce        sync.Once
	workerDone       bool
	shutdownEnded    bool
	shutdownErr      error
	terminateStarted bool
	terminateErr     error
	killAttempted    bool
	childWaitEnded   bool
	childWaitErr     error
}

func newActorShutdown(handle *sessionHandle, workerSettled <-chan struct{}) *actorShutdown {
	var session provider.Session
	var child provider.ManagedChild
	if handle != nil {
		session, child = handle.session, handle.child
	}
	return &actorShutdown{
		session: session, child: child, workerSettled: workerSettled,
		shutdownDone: make(chan error, 1), workerDone: workerSettled == nil,
	}
}

func (attempt *actorShutdown) run(ctx context.Context, timeout time.Duration) error {
	if attempt == nil || common.IsNil(attempt.session) || timeout <= 0 {
		return errors.New("invalid actor shutdown")
	}
	runCtx, runCancel := context.WithTimeout(ctx, timeout)
	defer runCancel()
	gracePeriod := timeout / 2
	if gracePeriod <= 0 {
		gracePeriod = timeout
	}
	// A retry resumes the persisted escalation phase. Shutdown starts only once,
	// so waiting through another grace window would spend the retry budget on a
	// provider call that cannot be restarted.
	if !attempt.terminateStarted {
		graceCtx, graceCancel := context.WithTimeout(runCtx, gracePeriod)
		attempt.startOnce.Do(func() {
			go func() {
				attempt.shutdownDone <- attempt.session.Shutdown(graceCtx)
			}()
		})

		graceExpired := false
		for (!attempt.workerDone || !attempt.shutdownEnded) && !graceExpired {
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
				graceExpired = true
			}
		}
		graceCancel()
		if attempt.workerDone && attempt.shutdownEnded && attempt.shutdownErr == nil {
			return nil
		}
	}

	child := attempt.child
	if common.IsNil(child) {
		return errors.New("provider shutdown failed without a managed child")
	}
	if !attempt.terminateStarted {
		attempt.terminateErr = child.Terminate()
		attempt.terminateStarted = true
	}
	if attempt.childWaitDone == nil && !attempt.childWaitEnded {
		attempt.childWaitDone = make(chan error, 1)
		go func(done chan<- error) { done <- child.Wait() }(attempt.childWaitDone)
	}
	if !attempt.childWaitEnded {
		select {
		case attempt.childWaitErr = <-attempt.childWaitDone:
			attempt.childWaitEnded = true
			attempt.childWaitDone = nil
		default:
		}
	}

	if attempt.terminateErr == nil && !attempt.killAttempted && !attempt.childWaitEnded {
		terminatePeriod := (timeout - gracePeriod) / 2
		if terminatePeriod <= 0 {
			terminatePeriod = gracePeriod
		}
		terminateCtx, terminateCancel := context.WithTimeout(runCtx, terminatePeriod)
		select {
		case attempt.childWaitErr = <-attempt.childWaitDone:
			attempt.childWaitEnded = true
			attempt.childWaitDone = nil
		case <-terminateCtx.Done():
			terminateCancel()
			select {
			case attempt.childWaitErr = <-attempt.childWaitDone:
				attempt.childWaitEnded = true
				attempt.childWaitDone = nil
			default:
			}
			if attempt.childWaitEnded {
				goto join
			}
			if runCtx.Err() != nil {
				return errors.New("provider shutdown did not join")
			}
			goto forceKill
		}
		terminateCancel()
	}
	if attempt.childWaitEnded {
		goto join
	}

forceKill:
	attempt.killAttempted = true
	if err := child.Kill(); err != nil {
		return errors.New("provider shutdown escalation failed")
	}

join:
	// Child exit must unblock both provider calls before actor ownership ends.
	for !attempt.childWaitEnded || !attempt.workerDone || !attempt.shutdownEnded {
		select {
		case attempt.childWaitErr = <-attempt.childWaitDone:
			attempt.childWaitEnded = true
			attempt.childWaitDone = nil
		case <-attempt.workerSettled:
			attempt.workerDone = true
			attempt.workerSettled = nil
		case attempt.shutdownErr = <-attempt.shutdownDone:
			attempt.shutdownEnded = true
			attempt.shutdownDone = nil
		case <-runCtx.Done():
			return errors.New("provider shutdown did not join")
		}
	}
	return nil
}
