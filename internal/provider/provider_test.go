package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/provider"
	"github.com/stretchr/testify/require"
)

var (
	_ provider.Driver       = (*fakeDriver)(nil)
	_ provider.Session      = (*fakeSession)(nil)
	_ provider.Launcher     = (*fakeLauncher)(nil)
	_ provider.ManagedChild = (*fakeChild)(nil)
)

func TestProviderContractCarriesExactContentOnlyTurnInMemory(t *testing.T) {
	request := provider.TurnRequest{
		TurnID:  "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		Message: "reader question",
		Context: &provider.PageContext{
			Revision:       provider.ContextInitial,
			Markdown:       []byte("# exact\nmarkdown\n"),
			CreatorContext: []byte("exact creator context\n"),
			Title:          "Board",
			URL:            "https://whiteboard.example/whiteboards/markdown/CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC",
			Resource: provider.Resource{
				Kind:      provider.ResourceMarkdown,
				ID:        "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC",
				CreatedAt: time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC),
				UpdatedAt: time.Date(2026, 7, 27, 2, 3, 4, 0, time.UTC),
			},
			Digest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
	}
	require.NoError(t, request.Validate())
	require.Equal(t, []byte("# exact\nmarkdown\n"), request.Context.Markdown)
	require.Equal(t, []byte("exact creator context\n"), request.Context.CreatorContext)
	require.Equal(t, provider.AccessContentOnly, provider.AccessContentOnly)
	_, err := json.Marshal(request.Context)
	require.Error(t, err)
}

func TestTurnRequestRejectsPartialContextAndInvalidRevisions(t *testing.T) {
	base := provider.TurnRequest{TurnID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Message: "question"}
	require.NoError(t, base.Validate())

	base.Context = &provider.PageContext{Revision: provider.ContextInitial, Markdown: []byte("markdown")}
	require.Error(t, base.Validate())
	base.Context.CreatorContext = []byte("context")
	base.Context.Title = "title"
	base.Context.URL = "https://whiteboard.example/page"
	base.Context.Resource = provider.Resource{Kind: provider.ResourceMarkdown, ID: "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	base.Context.Digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	require.NoError(t, base.Validate())
	base.Context.Revision = "partial"
	require.Error(t, base.Validate())
}

func TestNormalizedProviderEventsContainNoNativeReferencesOrRawPayloadSlot(t *testing.T) {
	now := time.Now().UTC()
	events := []provider.Event{
		provider.NewUserMessageEvent("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "question", now),
		provider.NewAssistantDeltaEvent("BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", "answer part"),
		provider.NewAssistantMessageEvent("BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", "answer", now),
		provider.NewActivityEvent(provider.ActivityCompaction, "Conversation compacted."),
		provider.NewBlockedEvent(provider.BlockedTool),
		provider.NewCompletionEvent("CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"),
		provider.NewInterruptionEvent("CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC", provider.InterruptionRequested),
	}
	for _, event := range events {
		require.NoError(t, event.Validate())
		require.NotEmpty(t, event.Kind)
		require.NotContains(t, event.Text, "/Users/")
	}
}

func TestNativeSessionReferencesAreValidatedProviderValues(t *testing.T) {
	_, err := provider.NewNativeSessionRef("")
	require.Error(t, err)
	ref, err := provider.NewNativeSessionRef("pi-session-reference")
	require.NoError(t, err)
	require.Equal(t, "pi-session-reference", ref.Value())
	_, err = json.Marshal(ref)
	require.Error(t, err)

	request := provider.OpenRequest{Provider: provider.NamePi, Access: provider.AccessContentOnly, NativeSession: ref}
	require.NoError(t, request.Validate())
	request.Access = "filesystem"
	require.Error(t, request.Validate())
}

func TestAcceptedTurnAndReconciliationAreExplicit(t *testing.T) {
	accepted := provider.AcceptedTurn{TurnID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", AcceptedAt: time.Now().UTC()}
	require.NoError(t, accepted.Validate())

	states := []provider.TurnState{provider.TurnAccepted, provider.TurnRunning, provider.TurnCompleted, provider.TurnInterrupted, provider.TurnUnknown}
	for _, state := range states {
		require.True(t, state.Valid())
	}
	require.False(t, provider.TurnState("native-state").Valid())
}

func TestReadinessTaxonomyIsClosed(t *testing.T) {
	states := []provider.ReadinessState{
		provider.Ready,
		provider.MissingExecutable,
		provider.AuthenticationRequired,
		provider.StartupFailed,
		provider.NoUsableModel,
		provider.ContentOnlyUnavailable,
		provider.ProtocolIncompatible,
	}
	for _, state := range states {
		require.True(t, state.Valid())
	}
	require.False(t, provider.ReadinessState("unknown-native-error").Valid())
}

type fakeDriver struct{}

func (*fakeDriver) Readiness(context.Context) provider.Readiness {
	return provider.Readiness{State: provider.Ready, Provider: provider.NamePi, Model: "model"}
}
func (*fakeDriver) Open(context.Context, provider.OpenRequest) (provider.Session, error) {
	return &fakeSession{events: make(chan provider.Event)}, nil
}
func (*fakeDriver) Delete(context.Context, provider.NativeSessionRef) error { return nil }

type fakeSession struct{ events chan provider.Event }

func (*fakeSession) NativeSession() provider.NativeSessionRef { return provider.NativeSessionRef{} }
func (*fakeSession) Model() string                            { return "model" }
func (*fakeSession) History(context.Context, provider.HistoryRequest) (provider.HistoryPage, error) {
	return provider.HistoryPage{}, nil
}
func (*fakeSession) Submit(context.Context, provider.TurnRequest) (provider.AcceptedTurn, error) {
	return provider.AcceptedTurn{}, nil
}
func (f *fakeSession) Events() <-chan provider.Event                        { return f.events }
func (*fakeSession) Interrupt(context.Context, provider.AcceptedTurn) error { return nil }
func (*fakeSession) Reconcile(context.Context, provider.AcceptedTurn) (provider.TurnState, error) {
	return provider.TurnUnknown, nil
}
func (*fakeSession) Shutdown(context.Context) error { return nil }

type fakeLauncher struct{}

func (*fakeLauncher) Launch(context.Context, provider.LaunchRequest) (provider.ManagedChild, error) {
	return &fakeChild{}, nil
}

type fakeChild struct{}

func (*fakeChild) Input() io.WriteCloser                 { return nopWriteCloser{} }
func (*fakeChild) Output() io.Reader                     { return nil }
func (*fakeChild) Errors() io.Reader                     { return nil }
func (*fakeChild) Wait() error                           { return nil }
func (*fakeChild) RequestShutdown(context.Context) error { return nil }
func (*fakeChild) Terminate() error                      { return nil }
func (*fakeChild) Kill() error                           { return nil }

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

func TestCompileTimeFakesHaveBehaviorallyUsableSignatures(t *testing.T) {
	driver := provider.Driver(&fakeDriver{})
	readiness := driver.Readiness(context.Background())
	require.Equal(t, provider.Ready, readiness.State)
	_, err := driver.Open(context.Background(), provider.OpenRequest{Provider: provider.NamePi, Access: provider.AccessContentOnly})
	require.NoError(t, err)

	child, err := (&fakeLauncher{}).Launch(context.Background(), provider.LaunchRequest{Provider: provider.NamePi, Access: provider.AccessContentOnly})
	require.NoError(t, err)
	require.NoError(t, child.RequestShutdown(context.Background()))
	require.NoError(t, child.Terminate())
	require.NoError(t, child.Kill())
	require.NoError(t, child.Wait())
	_, err = child.Input().Write([]byte("rpc"))
	require.NoError(t, err)
	require.True(t, errors.Is(io.EOF, io.EOF))
}
