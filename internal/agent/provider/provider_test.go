package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent"
	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

const (
	idA = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	idB = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	idC = "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
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

	request.Content = provider.TextMessage(strings.Repeat("x", provider.MaxTurnMessageBytes+1))
	require.Error(t, request.Validate())
	request = validTurnRequest()
	request.Context.Markdown = []byte(strings.Repeat("x", provider.MaxMarkdownBytes+1))
	require.Error(t, request.Validate())
	request = validTurnRequest()
	request.Context.CreatorContext = []byte(strings.Repeat("x", provider.MaxCreatorContextBytes+1))
	require.Error(t, request.Validate())
}

func TestTurnRequestRejectsPartialContextInvalidRevisionAndUTF8(t *testing.T) {
	base := provider.TurnRequest{TurnID: idA, MessageID: idB, Content: provider.TextMessage("question")}
	require.NoError(t, base.Validate())

	base.Context = &provider.PageContext{Revision: provider.ContextInitial, Markdown: []byte("markdown")}
	require.Error(t, base.Validate())
	base.Context = validTurnRequest().Context
	base.Context.Revision = "partial"
	require.Error(t, base.Validate())
	base = validTurnRequest()
	base.Content = provider.TextMessage(string([]byte{0xff}))
	require.Error(t, base.Validate())
}

func TestNormalizedProviderEventsCarryTurnIdentityAndTypedTerminalFailure(t *testing.T) {
	now := time.Now().UTC()
	failure := provider.NewProviderError(provider.ErrorMalformedStream)
	events := []provider.Event{
		provider.NewUserMessageEvent(idA, idB, provider.TextMessage("question"), now),
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

func TestToolActivityAndInteractiveRequestContractsAreBoundedAndTyped(t *testing.T) {
	activity := provider.ToolActivity{
		ID: idC, TurnID: idA, Kind: provider.ToolCommand, Status: provider.ToolRunning,
		Title: "Run tests", Summary: "Running a focused test", Detail: "go test ./internal/agent/codex",
	}
	require.NoError(t, activity.Validate())
	request := provider.InteractionRequest{
		ID: idC, TurnID: idA, Kind: provider.InteractionCommandApproval,
		Title: "Approve command", Summary: "Run the focused tests?", Command: "go test ./internal/agent/codex", WorkingDirectory: "/workspace",
		Options: []provider.InteractionOption{
			{ID: "accept", Label: "Allow once", Description: "Run this command."},
			{ID: "decline", Label: "Decline", Description: "Continue without running it."},
		},
	}
	require.NoError(t, request.Validate())
	require.NoError(t, provider.NewInteractionRequestEvent(request).Validate())
	resolution := provider.InteractionResolution{RequestID: request.ID, Kind: request.Kind, OptionID: "accept"}
	require.NoError(t, resolution.Validate())
	require.NoError(t, provider.NewInteractionResolvedEvent(resolution).Validate())

	response := provider.InteractionResponse{RequestID: request.ID, Kind: request.Kind, OptionID: "accept"}
	require.NoError(t, response.Validate())
	var interactive provider.InteractiveSession = validInteractiveFakeSession()
	require.NoError(t, interactive.Respond(context.Background(), response))

	request.Options[0].ID = "bad id"
	require.Error(t, request.Validate())
	resolution.OptionID = "bad id"
	require.Error(t, resolution.Validate())
	response.RequestID = "native-id"
	require.Error(t, response.Validate())
}

func TestStructuredInteractionQuestionsValidateAnswersWithoutNativePayloads(t *testing.T) {
	request := provider.InteractionRequest{
		ID: idC, TurnID: idA, Kind: provider.InteractionUserInput, Title: "Need input", Summary: "Choose a target.",
		Questions: []provider.InteractionQuestion{{
			ID: "target", Header: "Target", Prompt: "Which target?", AllowOther: true,
			Options: []provider.InteractionOption{{ID: "local", Label: "Local", Description: "Use the local target."}},
		}},
	}
	require.NoError(t, request.Validate())
	response := provider.InteractionResponse{RequestID: idC, Kind: provider.InteractionUserInput, Answers: map[string][]string{"target": {"local"}}}
	require.NoError(t, response.Validate())
	response.Answers["target"] = []string{strings.Repeat("x", provider.MaxInteractionTextBytes+1)}
	require.Error(t, response.Validate())
}

func TestStructuredInteractionRequestsRejectUnanswerableSurfaces(t *testing.T) {
	request := provider.InteractionRequest{
		ID: idC, TurnID: idA, Kind: provider.InteractionUserInput, Title: "Need input",
		Questions: []provider.InteractionQuestion{{ID: "target", Header: "Target", Prompt: "Which target?"}},
	}
	require.Error(t, request.Validate())

	request.Questions[0].AllowOther = true
	require.NoError(t, request.Validate())

	request = provider.InteractionRequest{
		ID: idC, TurnID: idA, Kind: provider.InteractionMCPElicitation, Title: "Need input",
		Options: []provider.InteractionOption{{ID: "accept", Label: "Accept"}, {ID: "decline", Label: "Decline"}, {ID: "cancel", Label: "Cancel"}},
		Fields:  []provider.InteractionField{{ID: "target", Label: "Target", Type: provider.InteractionSelect}},
	}
	require.Error(t, request.Validate())

	request.Fields[0].Options = []provider.InteractionOption{{ID: "local", Label: "Local"}}
	require.NoError(t, request.Validate())

	request.Fields[0].Type = provider.InteractionText
	require.Error(t, request.Validate())
}

func TestInteractionRequestsRequireKindSpecificResponseSurfaces(t *testing.T) {
	option := func(id string) provider.InteractionOption { return provider.InteractionOption{ID: id, Label: id} }
	tests := []provider.InteractionRequest{
		{ID: idC, Kind: provider.InteractionCommandApproval, Title: "Command", Questions: []provider.InteractionQuestion{{ID: "answer", Header: "Answer", Prompt: "Answer?", AllowOther: true}}},
		{ID: idC, Kind: provider.InteractionPermissionApproval, Title: "Permission", Options: []provider.InteractionOption{option("accept")}, Fields: []provider.InteractionField{{ID: "permissions", Label: "Permissions", Type: provider.InteractionMultiSelect, Options: []provider.InteractionOption{option("read")}}}},
		{ID: idC, Kind: provider.InteractionUserInput, Title: "Input", Options: []provider.InteractionOption{option("accept")}},
		{ID: idC, Kind: provider.InteractionMCPElicitation, Title: "MCP", Fields: []provider.InteractionField{{ID: "name", Label: "Name", Type: provider.InteractionText}}},
	}
	for _, request := range tests {
		require.Error(t, request.Validate(), request.Kind)
	}

	permission := provider.InteractionRequest{
		ID: idC, Kind: provider.InteractionPermissionApproval, Title: "Permission",
		Options: []provider.InteractionOption{option("grantTurn"), option("grantSession"), option("decline")},
		Fields:  []provider.InteractionField{{ID: "permissions", Label: "Permissions", Type: provider.InteractionMultiSelect, Options: []provider.InteractionOption{option("read")}}},
	}
	require.NoError(t, permission.Validate())
	mcp := provider.InteractionRequest{
		ID: idC, Kind: provider.InteractionMCPElicitation, Title: "MCP",
		Options: []provider.InteractionOption{option("accept"), option("decline"), option("cancel")},
		Fields:  []provider.InteractionField{{ID: "name", Label: "Name", Type: provider.InteractionText}},
	}
	require.NoError(t, mcp.Validate())
}

func TestProviderEventsRejectEveryFieldNotPermittedByKind(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	failure := provider.NewProviderError(provider.ErrorMalformedStream)
	tests := []struct {
		name    string
		event   provider.Event
		allowed map[string]bool
	}{
		{"user message", provider.NewUserMessageEvent(idA, idB, provider.TextMessage("question"), now), map[string]bool{"MessageID": true, "TurnID": true, "Content": true, "Timestamp": true}},
		{"assistant delta", provider.NewAssistantDeltaEvent(idA, idB, "part"), map[string]bool{"MessageID": true, "TurnID": true, "Text": true}},
		{"assistant message", provider.NewAssistantMessageEvent(idA, idB, "answer", now), map[string]bool{"MessageID": true, "TurnID": true, "Text": true, "Timestamp": true}},
		{"activity", provider.NewActivityEvent(idA, provider.ActivityStatus, "working"), map[string]bool{"TurnID": true, "Text": true, "Activity": true}},
		{"blocked", provider.NewBlockedEvent(idA, provider.BlockedTool), map[string]bool{"TurnID": true, "Text": true, "Blocked": true}},
		{"completion", provider.NewCompletionEvent(idA), map[string]bool{"TurnID": true}},
		{"interruption", provider.NewInterruptionEvent(idA, provider.InterruptionRequested), map[string]bool{"TurnID": true, "Interruption": true}},
		{"terminal failure", provider.NewTerminalFailureEvent(idA, failure), map[string]bool{"TurnID": true, "Failure": true}},
	}
	mutations := map[string]func(*provider.Event){
		"MessageID":    func(event *provider.Event) { event.MessageID = idB },
		"TurnID":       func(event *provider.Event) { event.TurnID = idA },
		"Text":         func(event *provider.Event) { event.Text = "contradictory text" },
		"Content":      func(event *provider.Event) { event.Content = provider.TextMessage("contradictory content") },
		"Timestamp":    func(event *provider.Event) { event.Timestamp = now },
		"Activity":     func(event *provider.Event) { event.Activity = provider.ActivityStatus },
		"Blocked":      func(event *provider.Event) { event.Blocked = provider.BlockedTool },
		"Interruption": func(event *provider.Event) { event.Interruption = provider.InterruptionRequested },
		"Failure":      func(event *provider.Event) { event.Failure = failure },
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, test.event.Validate())
			for field, mutate := range mutations {
				if test.allowed[field] {
					continue
				}
				t.Run(field, func(t *testing.T) {
					contradictory := test.event
					mutate(&contradictory)
					require.Error(t, contradictory.Validate())
				})
			}
		})
	}
}

func TestProviderPageContextDigestAndAuthorizedHostnameAreValidated(t *testing.T) {
	request := validTurnRequest()
	require.NoError(t, request.Context.Validate())
	request.Context.URL = "http://127.0.0.1:8080/whiteboards/markdown/CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
	require.NoError(t, request.Context.Validate())

	exactControlBytes := *request.Context
	exactControlBytes.Markdown = []byte("# exact\n\x00\x01")
	exactControlBytes.CreatorContext = []byte("creator\n\x02\x03")
	exactControlBytes.Digest = agent.CalculateContextDigest(exactControlBytes.Markdown, exactControlBytes.CreatorContext)
	require.NoError(t, exactControlBytes.Validate(), "valid published UTF-8 artifacts must preserve exact control bytes")

	request.Context.Digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	require.Error(t, request.Context.Validate())

	for name, value := range map[string]string{
		"empty hostname":       "https://:443/path",
		"credentials":          "https://user:pass@whiteboard.example/path",
		"malformed authority":  "https://[::1/path",
		"localhost HTTP":       "http://localhost:8080/path",
		"default HTTP port":    "http://127.0.0.1:80/path",
		"noncanonical port":    "http://127.0.0.1:080/path",
		"other loopback HTTP":  "http://127.0.0.2:8080/path",
		"IPv6 loopback HTTP":   "http://[::1]:8080/path",
		"remote HTTP":          "http://whiteboard.example/path",
		"loopback credentials": "http://user@127.0.0.1:8080/path",
	} {
		t.Run(name, func(t *testing.T) {
			context := *validTurnRequest().Context
			context.URL = value
			require.Error(t, context.Validate())
		})
	}
}

func TestHistoryPageNextCursorMatchesLastReturnedMessage(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	item := provider.HistoryItem{TurnID: idA, MessageID: idB, Role: provider.HistoryUser, Content: provider.TextMessage("question"), CreatedAt: now}
	require.NoError(t, (provider.HistoryPage{Items: []provider.HistoryItem{item}, NextCursor: idB}).Validate())
	require.NoError(t, (provider.HistoryPage{Items: []provider.HistoryItem{item}}).Validate())
	require.Error(t, (provider.HistoryPage{Items: []provider.HistoryItem{item}, NextCursor: idA}).Validate())
	require.Error(t, (provider.HistoryPage{Items: []provider.HistoryItem{}, NextCursor: idA}).Validate())
}

func TestProviderUserVisibleTextRejectsDisallowedC0Controls(t *testing.T) {
	for control := byte(0); control < 0x20; control++ {
		if control == '\t' || control == '\n' || control == '\r' {
			continue
		}
		event := provider.NewAssistantDeltaEvent(idA, idB, "visible"+string(control))
		require.Error(t, event.Validate(), "control %#x", control)
	}
}

func TestNormalizedProviderEventAndHistoryBounds(t *testing.T) {
	now := time.Now().UTC()
	require.Error(t, provider.NewActivityEvent(idA, provider.ActivityStatus, strings.Repeat("s", provider.MaxSummaryBytes+1)).Validate())
	require.Error(t, provider.NewAssistantMessageEvent(idA, idB, strings.Repeat("m", provider.MaxEventTextBytes+1), now).Validate())

	page := provider.HistoryPage{Items: []provider.HistoryItem{{TurnID: idB, MessageID: idA, Role: provider.HistoryUser, Content: provider.TextMessage(strings.Repeat("h", provider.MaxHistoryItemBytes)), CreatedAt: now}}}
	require.NoError(t, page.Validate())
	page.Items[0].Content = provider.TextMessage(strings.Repeat("h", provider.MaxHistoryItemBytes+1))
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
	workspace := t.TempDir()

	create := provider.CreateRequest{Provider: provider.NamePi, Access: provider.AccessConfigured, Workspace: workspace}
	require.NoError(t, create.Validate())
	resume := provider.ResumeRequest{Provider: provider.NamePi, Access: provider.AccessConfigured, NativeSession: ref, Workspace: workspace}
	require.NoError(t, resume.Validate())
	inspect := provider.InspectRequest{Provider: provider.NamePi, NativeSession: ref}
	require.NoError(t, inspect.Validate())
	deleteRequest := provider.DeleteRequest{Provider: provider.NamePi, NativeSession: ref}
	require.NoError(t, deleteRequest.Validate())

	create.Workspace = "relative/workspace"
	require.Error(t, create.Validate())
	resume.NativeSession = provider.NativeSessionRef{}
	require.Error(t, resume.Validate())
	resume.NativeSession = ref
	resume.Workspace = "relative/workspace"
	require.Error(t, resume.Validate())
	inspect.NativeSession = provider.NativeSessionRef{}
	require.Error(t, inspect.Validate())
	deleteRequest.NativeSession = provider.NativeSessionRef{}
	require.Error(t, deleteRequest.Validate())
}

func TestProviderNamesAndAccessModesAreClosed(t *testing.T) {
	for _, name := range []provider.Name{provider.NamePi, provider.NameCodex} {
		require.True(t, name.Valid())
	}
	require.False(t, provider.Name("other").Valid())
	require.Equal(t, []provider.Name{provider.NamePi, provider.NameCodex}, provider.AllNames())

	workspace := t.TempDir()
	require.NoError(t, (provider.CreateRequest{Provider: provider.NamePi, Access: provider.AccessConfigured, Workspace: workspace}).Validate())
	require.NoError(t, (provider.CreateRequest{Provider: provider.NameCodex, Access: provider.AccessConfigured, Workspace: workspace}).Validate())
	require.Error(t, (provider.CreateRequest{Provider: provider.NamePi, Access: provider.AccessContentOnly, Workspace: workspace}).Validate())
	require.Error(t, (provider.CreateRequest{Provider: provider.NameCodex, Access: provider.AccessContentOnly, Workspace: workspace}).Validate())
}

func TestRegistryRejectsUnknownOrNilDrivers(t *testing.T) {
	pi := &fakeDriver{}
	codex := &fakeDriver{name: provider.NameCodex}
	registry, err := provider.NewRegistry(map[provider.Name]provider.Driver{
		provider.NamePi: pi, provider.NameCodex: codex,
	})
	require.NoError(t, err)
	require.Same(t, pi, registry.Lookup(provider.NamePi))
	require.Same(t, codex, registry.Lookup(provider.NameCodex))
	require.Nil(t, registry.Lookup(provider.Name("other")))
	require.Equal(t, []provider.Name{provider.NamePi, provider.NameCodex}, registry.Names())

	_, err = provider.NewRegistry(map[provider.Name]provider.Driver{provider.Name("other"): pi})
	require.Error(t, err)
	_, err = provider.NewRegistry(map[provider.Name]provider.Driver{provider.NamePi: nil})
	require.Error(t, err)
}

func TestLaunchRequestCarriesAnExplicitProcessSpecification(t *testing.T) {
	workingDirectory := t.TempDir()
	valid := provider.LaunchRequest{
		Executable: workingDirectory + "/provider",
		Arguments:  []string{"--mode", "json"},
		Environment: []string{
			"HOME=" + workingDirectory,
		},
		WorkingDirectory: workingDirectory,
	}
	require.NoError(t, valid.Validate())

	invalid := []provider.LaunchRequest{
		{},
		{Executable: "relative/provider", Arguments: []string{}, Environment: []string{}, WorkingDirectory: workingDirectory},
		{Executable: valid.Executable, Arguments: nil, Environment: []string{}, WorkingDirectory: workingDirectory},
		{Executable: valid.Executable, Arguments: []string{}, Environment: nil, WorkingDirectory: workingDirectory},
		{Executable: valid.Executable, Arguments: []string{}, Environment: []string{}, WorkingDirectory: "relative/workspace"},
		{Executable: valid.Executable, Arguments: []string{"bad\x00argument"}, Environment: []string{}, WorkingDirectory: workingDirectory},
		{Executable: valid.Executable, Arguments: []string{}, Environment: []string{"BAD\x00=value"}, WorkingDirectory: workingDirectory},
		{Executable: valid.Executable, Arguments: []string{}, Environment: []string{"MISSING_EQUALS"}, WorkingDirectory: workingDirectory},
		{Executable: valid.Executable, Arguments: []string{}, Environment: []string{"=empty-name"}, WorkingDirectory: workingDirectory},
		{Executable: valid.Executable, Arguments: []string{}, Environment: []string{"HOME=/first", "HOME=/second"}, WorkingDirectory: workingDirectory},
		{Executable: valid.Executable, Arguments: make([]string, provider.MaxLaunchItems+1), Environment: []string{}, WorkingDirectory: workingDirectory},
		{Executable: valid.Executable, Arguments: []string{strings.Repeat("x", provider.MaxLaunchAggregateBytes+1)}, Environment: []string{}, WorkingDirectory: workingDirectory},
	}
	for _, request := range invalid {
		require.Error(t, request.Validate())
	}
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
		provider.ErrorContextTooLarge, provider.ErrorAcceptanceUnknown, provider.ErrorInvalidModelConfiguration,
		provider.ErrorImageInputUnsupported, provider.ErrorImageUnsupported, provider.ErrorImageTooLarge,
		provider.ErrorImageTurnLimit, provider.ErrorImageMissing, provider.ErrorImageStorageFailure,
		provider.ErrorSkillUnavailable, provider.ErrorCompactUnsupported,
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
	reference := provider.TurnReference{TurnID: idA}
	require.NoError(t, reference.Validate())
	reference.TurnID = "invalid"
	require.Error(t, reference.Validate())
	states := []provider.TurnState{provider.TurnNotAccepted, provider.TurnAccepted, provider.TurnRunning, provider.TurnCompleted, provider.TurnInterrupted, provider.TurnUnknown}
	for _, state := range states {
		require.True(t, state.Valid())
	}
	require.True(t, provider.TurnNotAccepted.Definitive())
	require.True(t, provider.TurnAccepted.Definitive())
	require.False(t, provider.TurnUnknown.Definitive())
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

type fakeDriver struct{ name provider.Name }

func (driver *fakeDriver) Readiness(context.Context) provider.Readiness {
	name := driver.name
	if name == "" {
		name = provider.NamePi
	}
	return provider.Readiness{State: provider.Ready, Provider: name, Model: "resolved-model"}
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
	child        provider.ManagedChild
}

type interactiveFakeSession struct{ *fakeSession }

func validInteractiveFakeSession() *interactiveFakeSession {
	return &interactiveFakeSession{fakeSession: validFakeSession()}
}

func (*interactiveFakeSession) Respond(context.Context, provider.InteractionResponse) error {
	return nil
}
func (*interactiveFakeSession) CancelInteraction(context.Context, string) error { return nil }

func validFakeSession() *fakeSession {
	return &fakeSession{events: make(chan provider.Event), metadata: validNativeSession(), child: &fakeChild{}}
}
func (*fakeSession) Model() string                           { return "resolved-model" }
func (*fakeSession) Capabilities() provider.Capabilities     { return provider.Capabilities{} }
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
func (*fakeSession) Reconcile(context.Context, provider.TurnReference) (provider.TurnState, error) {
	return provider.TurnUnknown, nil
}
func (f *fakeSession) Child() provider.ManagedChild { return f.child }
func (*fakeSession) Shutdown(context.Context) error { return nil }

type fakeLauncher struct{}

func (*fakeLauncher) Launch(context.Context, provider.LaunchRequest) (provider.ManagedChild, error) {
	return &fakeChild{}, nil
}

type fakeChild struct{}

func (*fakeChild) Input() io.WriteCloser { return nopWriteCloser{} }
func (*fakeChild) Output() io.Reader     { return nil }
func (*fakeChild) Errors() io.Reader     { return nil }
func (*fakeChild) Wait() error           { return nil }
func (*fakeChild) Terminate() error      { return nil }
func (*fakeChild) Kill() error           { return nil }

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

func TestCompileTimeFakesHaveBehaviorallyUsableSignatures(t *testing.T) {
	driver := provider.Driver(&fakeDriver{})
	readiness := driver.Readiness(context.Background())
	require.NoError(t, readiness.Validate())

	workspace := t.TempDir()
	created, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NamePi, Access: provider.AccessConfigured, Workspace: workspace})
	require.NoError(t, err)
	require.NoError(t, created.NativeSession().Validate())
	require.NotNil(t, created.Child())

	ref := created.NativeSession().Ref
	resumed, err := driver.Resume(context.Background(), provider.ResumeRequest{Provider: provider.NamePi, Access: provider.AccessConfigured, NativeSession: ref, Workspace: workspace})
	require.NoError(t, err)
	require.NoError(t, resumed.NativeSession().Validate())
	state, err := resumed.Reconcile(context.Background(), provider.TurnReference{TurnID: idA})
	require.NoError(t, err)
	require.True(t, state.Valid())
	metadata, err := driver.Inspect(context.Background(), provider.InspectRequest{Provider: provider.NamePi, NativeSession: ref})
	require.NoError(t, err)
	require.NoError(t, metadata.Validate())
	require.NoError(t, driver.Delete(context.Background(), provider.DeleteRequest{Provider: provider.NamePi, NativeSession: ref}))

	workingDirectory := t.TempDir()
	child, err := (&fakeLauncher{}).Launch(context.Background(), provider.LaunchRequest{
		Executable: workingDirectory + "/provider", Arguments: []string{}, Environment: []string{}, WorkingDirectory: workingDirectory,
	})
	require.NoError(t, err)
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
		TurnID: idA, MessageID: idB, Content: provider.TextMessage("reader question"),
		Context: &provider.PageContext{
			Revision: provider.ContextInitial, Markdown: []byte("# exact\nmarkdown\n"), CreatorContext: []byte("exact creator context\n"), Title: "Board",
			URL:      "https://whiteboard.example/whiteboards/markdown/CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC",
			Resource: provider.Resource{Kind: provider.ResourceMarkdown, ID: "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC", CreatedAt: now, UpdatedAt: now},
			Digest:   agent.CalculateContextDigest([]byte("# exact\nmarkdown\n"), []byte("exact creator context\n")),
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
