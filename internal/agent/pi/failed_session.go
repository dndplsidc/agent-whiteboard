//go:build unix

package pi

import (
	"context"
	"io"
	"sync"

	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
)

// failedSession transfers ownership of a child returned on a failed launch
// before a usable RPC client exists. It intentionally exposes no events or
// operations, but gives the broker a valid, retryably stoppable process handle.
type failedSession struct {
	driver *Driver
	native provider.NativeSession
	child  provider.ManagedChild
	events chan provider.Event

	closeOnce sync.Once
	waitOnce  sync.Once
	waitDone  chan struct{}
}

func newFailedSession(driver *Driver, native provider.NativeSession, child provider.ManagedChild) *failedSession {
	events := make(chan provider.Event)
	close(events)
	return &failedSession{driver: driver, native: native, child: child, events: events, waitDone: make(chan struct{})}
}

func (session *failedSession) NativeSession() provider.NativeSession { return session.native }
func (session *failedSession) Model() string                         { return session.native.Model }
func (*failedSession) Capabilities() provider.Capabilities           { return provider.Capabilities{} }
func (session *failedSession) Events() <-chan provider.Event         { return session.events }
func (session *failedSession) Child() provider.ManagedChild          { return session.child }
func (*failedSession) History(context.Context, provider.HistoryRequest) (provider.HistoryPage, error) {
	return provider.HistoryPage{}, provider.NewProviderError(provider.ErrorProtocolFailure)
}
func (*failedSession) Preflight(context.Context, provider.PreflightRequest) (provider.PreflightResult, error) {
	return provider.PreflightResult{}, provider.NewProviderError(provider.ErrorProtocolFailure)
}
func (*failedSession) Submit(context.Context, provider.TurnRequest) (provider.AcceptedTurn, error) {
	return provider.AcceptedTurn{}, provider.NewProviderError(provider.ErrorProtocolFailure)
}
func (*failedSession) Interrupt(context.Context, provider.AcceptedTurn) error {
	return provider.NewProviderError(provider.ErrorProtocolFailure)
}
func (*failedSession) Reconcile(context.Context, provider.TurnReference) (provider.TurnState, error) {
	return provider.TurnUnknown, provider.NewProviderError(provider.ErrorProtocolFailure)
}
func (session *failedSession) Shutdown(ctx context.Context) error {
	session.closeOnce.Do(func() {
		if input := session.child.Input(); input != nil {
			_ = input.Close()
		}
	})
	session.waitOnce.Do(func() {
		if errorsStream := session.child.Errors(); errorsStream != nil {
			go func() { _, _ = io.CopyBuffer(io.Discard, errorsStream, make([]byte, 32<<10)) }()
		}
		go func() {
			_ = session.child.Wait()
			if session.driver != nil {
				session.driver.release(session.native.Ref.Value(), session)
			}
			close(session.waitDone)
		}()
	})
	select {
	case <-session.waitDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var _ provider.Session = (*failedSession)(nil)
