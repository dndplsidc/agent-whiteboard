package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/provider"
	"github.com/stretchr/testify/require"
)

const (
	idA = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	idB = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
)

var (
	_ provider.Driver       = (*fakeDriver)(nil)
	_ provider.Session      = (*fakeSession)(nil)
	_ provider.Launcher     = (*fakeLauncher)(nil)
	_ provider.ManagedChild = (*fakeChild)(nil)
)

func TestProviderContractCarriesExactBoundedContentOnlyTurnInMemory(t *testing.T) {
	request := validTurnRequest()
	require.NoError(t, request.Validate())
	require.Equal(t, []byte("# exact\nmarkdown\n"), request.Context.Markdown)
	require.Equal(t, []byte("exact creator context\n"), request.Context.CreatorContext)
	_, err := json.Marshal(request.Context)
	require.Error(t, err)

	request.Message = strings.Repeat("x", provider.MaxTurnMessageBytes+1)
	require.Error(t, request.Validate())
	request = validTurnRequest()
	request.Context.Markdown = []byte(strings.Repeat("x", provider.MaxMarkdownBytes+1))
	require.Error(t, request.Validate())
	request = validTurnRequest()
	request.Context.CreatorContext = []byte(strings.Repeat("x", provider.MaxCreatorContextBytes+1))
	require.Error(t, request.Validate())
}

func TestTurnRequestRejectsPartialContextInvalidRevisionAndUTF8(t *testing.T) {
	base := provider.TurnRequest{TurnID: idA, MessageID: idB, Message: "question"}
	require.NoError(t, base.Validate())

	base.Context = &provider.PageContext{Revision: provider.ContextInitial, Markdown: []byte("markdown")}
	require.Error(t, base.Validate())
	base.Context = validTurnRequest().Context
	base.Context.Revision = "partial"
	require.Error(t, base.Validate())
	base = validTurnRequest()
	base.Message = string([]byte{0xff})
	require.Error(t, base.Validate())
}

func TestNormalizedProviderEventsCarryTurnIdentityAndTypedTerminalFailure(t *testing.T) {
	now := time.Now().UTC()
	failure := provider.NewProviderError(provider.ErrorMalformedStream)
	events := []provider.Event{
		provider.NewUserMessageEvent(idA, idB, "question", now),
		provider.NewAssistantDeltaEvent(idA, idB, "answer part"),
		provider.NewAssistantMessageEvent(idA, idB, "answer", now),
		provider.NewActivityEvent(idA, provider.ActivityCompaction, "Conversation compacted."),
		provider.NewBlockedEvent(idA, provider.BlockedTool),
		provider.NewCompletionEvent(idA),
		provider.NewInterruptionEvent(idA, provider.InterruptionRequested),
		provider.NewTerminalFailureEvent(idA, failure),
	}
	for _, event := range events {
		require.NoError(t, event.Validate())
		require.Equal(t, idA, event.TurnID)
		require.NotContains(t, event.Text, "/Users/")
	}
	terminal := events[len(events)-1]
	require.Equal(t, provider.EventTerminalFailure, terminal.Kind)
	require.Equal(t, provider.ErrorMalformedStream, terminal.Failure.Code())
	require.NoError(t, provider.NewTerminalFailureEvent("", failure).Validate())
	_, err := json.Marshal(terminal)
	require.Error(t, err)
}

func TestNormalizedProviderEventAndHistoryBounds(t *testing.T) {
	now := time.Now().UTC()
	require.Error(t, provider.NewActivityEvent(idA, provider.ActivityStatus, strings.Repeat("s", provider.MaxSummaryBytes+1)).Validate())
	require.Error(t, provider.NewAssistantMessageEvent(idA, idB, strings.Repeat("m", provider.MaxEventTextBytes+1), now).Validate())

	page := provider.HistoryPage{Items: []provider.HistoryItem{{TurnID: idB, MessageID: idA, Role: provider.HistoryUser, Text: strings.Repeat("h", provider.MaxHistoryItemBytes), CreatedAt: now}}}
	require.NoError(t, page.Validate())
	page.Items[0].Text += "x"
	require.Error(t, page.Validate())

	page.Items = make([]provider.HistoryItem, provider.MaxHistoryItems+1)
	require.Error(t, page.Validate())

	page.Items = make([]provider.HistoryItem, provider.MaxHistoryBytes/provider.MaxHistoryItemBytes+1)
	for index := range page.Items {
		page.Items[index] = provider.HistoryItem{TurnID: idB, MessageID: idA, Role: provider.HistoryAssistant, Text: strings.Repeat("h", provider.MaxHistoryItemBytes), CreatedAt: now}
	}
	require.Error(t, page.Validate())
}

func TestNativeSessionReferencesAndMetadataAreValidated(t *testing.T) {
	_, err := provider.NewNativeSessionRef("")
	require.Error(t, err)
	ref, err := provider.NewNativeSessionRef("pi-session-reference")
	require.NoError(t, err)
	_, err = json.Marshal(ref)
	require.Error(t, err)

	metadata := provider.NativeSession{Ref: ref, Provider: provider.NamePi, Model: "resolved-model", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	require.NoError(t, metadata.Validate())
	metadata.Ref = provider.NativeSessionRef{}
	require.Error(t, metadata.Validate())
}

func TestDriverLifecycleOperationsAreExplicitAndRejectZeroReferences(t *testing.T) {
	ref, err := provider.NewNativeSessionRef("pi-session-reference")
	require.NoError(t, err)

	create := provider.CreateRequest{Provider: provider.NamePi, Access: provider.AccessContentOnly}
	require.NoError(t, create.Validate())
	resume := provider.ResumeRequest{Provider: provider.NamePi, Access: provider.AccessContentOnly, NativeSession: ref}
	require.NoError(t, resume.Validate())
	inspect := provider.InspectRequest{Provider: provider.NamePi, NativeSession: ref}
	require.NoError(t, inspect.Validate())
	deleteRequest := provider.DeleteRequest{Provider: provider.NamePi, NativeSession: ref}
	require.NoError(t, deleteRequest.Validate())

	resume.NativeSession = provider.NativeSessionRef{}
	require.Error(t, resume.Validate())
	inspect.NativeSession = provider.NativeSessionRef{}
	require.Error(t, inspect.Validate())
	deleteRequest.NativeSession = provider.NativeSessionRef{}
	require.Error(t, deleteRequest.Validate())
}

func TestLaunchOperationDiscriminatorRejectsCreateReferenceAndMissingResumeReference(t *testing.T) {
	ref, err := provider.NewNativeSessionRef("pi-session-reference")
	require.NoError(t, err)
	require.NoError(t, (provider.LaunchRequest{Provider: provider.NamePi, Access: provider.AccessContentOnly, Operation: provider.LaunchCreate}).Validate())
	require.NoError(t, (provider.LaunchRequest{Provider: provider.NamePi, Access: provider.AccessContentOnly, Operation: provider.LaunchResume, NativeSession: ref}).Validate())
	require.Error(t, (provider.LaunchRequest{Provider: provider.NamePi, Access: provider.AccessContentOnly, Operation: provider.LaunchCreate, NativeSession: ref}).Validate())
	require.Error(t, (provider.LaunchRequest{Provider: provider.NamePi, Access: provider.AccessContentOnly, Operation: provider.LaunchResume}).Validate())
}

func TestPreflightExposesEffectiveSizingAndTypedContextTooLarge(t *testing.T) {
	request := provider.PreflightRequest{Turn: validTurnRequest()}
	require.NoError(t, request.Validate())
	result := provider.PreflightResult{ResolvedModel: "resolved-model", EstimatedInputTokens: 1_000, EffectiveCapacityTokens: 8_000, SafetyMarginTokens: 1_000}
	require.NoError(t, result.Validate())

	session := validFakeSession()
	session.preflight = result
	got, err := session.Preflight(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, result, got)

	session.preflightErr = provider.NewProviderError(provider.ErrorContextTooLarge)
	_, err = session.Preflight(context.Background(), request)
	var typed provider.ProviderError
	require.ErrorAs(t, err, &typed)
	require.Equal(t, provider.ErrorContextTooLarge, typed.Code())
}

func TestProviderErrorTaxonomyIsClosedStaticAndRedacted(t *testing.T) {
	codes := []provider.ProviderErrorCode{
		provider.ErrorNotReady, provider.ErrorReadinessFailed, provider.ErrorMissingExecutable, provider.ErrorStartupFailed, provider.ErrorAuthenticationRequired,
		provider.ErrorNoUsableModel, provider.ErrorContentOnlyUnavailable, provider.ErrorProtocolIncompatible,
		provider.ErrorProtocolFailure, provider.ErrorMalformedStream, provider.ErrorChildExited, provider.ErrorNativeSessionMissing,
		provider.ErrorContextTooLarge, provider.ErrorAcceptanceUnknown,
	}
	require.Equal(t, codes, provider.AllProviderErrorCodes())
	for _, code := range codes {
		err := provider.NewProviderError(code)
		require.Equal(t, code, err.Code())
		require.NotEmpty(t, err.Error())
		require.NotContains(t, err.Error(), "/")
	}
	require.False(t, provider.NewProviderError("native-secret").Valid())
}

func TestAcceptedTurnAndReconciliationAreExplicit(t *testing.T) {
	accepted := provider.AcceptedTurn{TurnID: idA, AcceptedAt: time.Now().UTC()}
	require.NoError(t, accepted.Validate())
	states := []provider.TurnState{provider.TurnAccepted, provider.TurnRunning, provider.TurnCompleted, provider.TurnInterrupted, provider.TurnUnknown}
	for _, state := range states {
		require.True(t, state.Valid())
	}
	require.False(t, provider.TurnState("native-state").Valid())
}

func TestReadinessTaxonomyAndResolvedModelAreClosed(t *testing.T) {
	states := []provider.ReadinessState{provider.Ready, provider.MissingExecutable, provider.AuthenticationRequired, provider.StartupFailed, provider.NoUsableModel, provider.ContentOnlyUnavailable, provider.ProtocolIncompatible}
	for _, state := range states {
		require.True(t, state.Valid())
	}
	require.False(t, provider.ReadinessState("unknown-native-error").Valid())
	require.Error(t, (provider.Readiness{State: provider.Ready, Provider: provider.NamePi}).Validate())
}

type fakeDriver struct{}

func (*fakeDriver) Readiness(context.Context) provider.Readiness {
	return provider.Readiness{State: provider.Ready, Provider: provider.NamePi, Model: "resolved-model"}
}
func (*fakeDriver) Create(context.Context, provider.CreateRequest) (provider.Session, error) {
	return validFakeSession(), nil
}
func (*fakeDriver) Resume(context.Context, provider.ResumeRequest) (provider.Session, error) {
	return validFakeSession(), nil
}
func (*fakeDriver) Inspect(context.Context, provider.InspectRequest) (provider.NativeSession, error) {
	return validNativeSession(), nil
}
func (*fakeDriver) Delete(context.Context, provider.DeleteRequest) error { return nil }

type fakeSession struct {
	events       chan provider.Event
	metadata     provider.NativeSession
	preflight    provider.PreflightResult
	preflightErr error
}

func validFakeSession() *fakeSession {
	return &fakeSession{events: make(chan provider.Event), metadata: validNativeSession()}
}
func (*fakeSession) Model() string                           { return "resolved-model" }
func (f *fakeSession) NativeSession() provider.NativeSession { return f.metadata }
func (*fakeSession) History(context.Context, provider.HistoryRequest) (provider.HistoryPage, error) {
	return provider.HistoryPage{Items: []provider.HistoryItem{}}, nil
}
func (f *fakeSession) Preflight(context.Context, provider.PreflightRequest) (provider.PreflightResult, error) {
	return f.preflight, f.preflightErr
}
func (*fakeSession) Submit(context.Context, provider.TurnRequest) (provider.AcceptedTurn, error) {
	return provider.AcceptedTurn{TurnID: idA, AcceptedAt: time.Now().UTC()}, nil
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
	require.NoError(t, readiness.Validate())

	created, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NamePi, Access: provider.AccessContentOnly})
	require.NoError(t, err)
	require.NoError(t, created.NativeSession().Validate())

	ref := created.NativeSession().Ref
	resumed, err := driver.Resume(context.Background(), provider.ResumeRequest{Provider: provider.NamePi, Access: provider.AccessContentOnly, NativeSession: ref})
	require.NoError(t, err)
	require.NoError(t, resumed.NativeSession().Validate())
	metadata, err := driver.Inspect(context.Background(), provider.InspectRequest{Provider: provider.NamePi, NativeSession: ref})
	require.NoError(t, err)
	require.NoError(t, metadata.Validate())
	require.NoError(t, driver.Delete(context.Background(), provider.DeleteRequest{Provider: provider.NamePi, NativeSession: ref}))

	child, err := (&fakeLauncher{}).Launch(context.Background(), provider.LaunchRequest{Provider: provider.NamePi, Access: provider.AccessContentOnly, Operation: provider.LaunchCreate})
	require.NoError(t, err)
	require.NoError(t, child.RequestShutdown(context.Background()))
	require.NoError(t, child.Terminate())
	require.NoError(t, child.Kill())
	require.NoError(t, child.Wait())
	_, err = child.Input().Write([]byte("rpc"))
	require.NoError(t, err)
	require.True(t, errors.Is(io.EOF, io.EOF))
}

func validTurnRequest() provider.TurnRequest {
	now := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	return provider.TurnRequest{
		TurnID: idA, MessageID: idB, Message: "reader question",
		Context: &provider.PageContext{
			Revision: provider.ContextInitial, Markdown: []byte("# exact\nmarkdown\n"), CreatorContext: []byte("exact creator context\n"), Title: "Board",
			URL:      "https://whiteboard.example/whiteboards/markdown/CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC",
			Resource: provider.Resource{Kind: provider.ResourceMarkdown, ID: "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC", CreatedAt: now, UpdatedAt: now},
			Digest:   "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
	}
}

func validNativeSession() provider.NativeSession {
	ref, err := provider.NewNativeSessionRef("pi-session-reference")
	if err != nil {
		panic(err)
	}
	now := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	return provider.NativeSession{Ref: ref, Provider: provider.NamePi, Model: "resolved-model", CreatedAt: now, UpdatedAt: now}
}
