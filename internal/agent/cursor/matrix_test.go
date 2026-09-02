package cursor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/acp"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
)

func matrixDriver(t *testing.T, scenarios ...scriptScenario) (*Driver, *scriptLauncher, string) {
	t.Helper()
	d, launcher, root := testDriver(t)
	launcher.scenarios = append([]scriptScenario(nil), scenarios...)
	return d, launcher, root
}

func matrixCaps(protocol int, load, list, image bool) any {
	var listValue any
	if list {
		listValue = map[string]any{}
	}
	return map[string]any{"protocolVersion": protocol, "agentCapabilities": map[string]any{
		"loadSession":         load,
		"promptCapabilities":  map[string]any{"image": image, "embeddedContext": false},
		"sessionCapabilities": map[string]any{"list": listValue},
	}}
}

func matrixRequest(t *testing.T, child *scriptChild, method string) json.RawMessage {
	t.Helper()
	child.mu.Lock()
	defer child.mu.Unlock()
	for _, request := range child.requests {
		if request.method == method {
			return append(json.RawMessage(nil), request.params...)
		}
	}
	t.Fatalf("request %q not recorded", method)
	return nil
}

func TestCursorMatrixReadinessCompatibilityAndAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name string
		init any
	}{
		{"protocol", matrixCaps(2, true, true, false)},
		{"load", matrixCaps(1, false, true, false)},
		{"list", matrixCaps(1, true, false, false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _, _ := matrixDriver(t, scriptScenario{initializeResult: tc.init})
			if got := d.Readiness(context.Background()); got.State != provider.ProtocolIncompatible {
				t.Fatalf("readiness = %+v", got)
			}
		})
	}
	t.Run("authentication is non-mutating", func(t *testing.T) {
		d, launcher, _ := matrixDriver(t, scriptScenario{initializeError: &acp.RPCError{Code: -32000, Message: "login"}})
		if got := d.Readiness(context.Background()); got.State != provider.AuthenticationRequired {
			t.Fatalf("readiness = %+v", got)
		}
		launcher.mu.Lock()
		child := launcher.children[0]
		launcher.mu.Unlock()
		child.mu.Lock()
		defer child.mu.Unlock()
		if len(child.methods) != 1 || child.methods[0] != "initialize" {
			t.Fatalf("authentication probe methods = %v", child.methods)
		}
	})
}

func TestCursorMatrixExactOpenParamsAndReplayBoundary(t *testing.T) {
	envelope := replayEnvelope(t, turnID, messageID, "loaded")
	update := func(text string) any {
		return map[string]any{"sessionId": "native-exact", "update": map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": text}}}
	}
	open := map[string]any{"sessionId": "native-exact", "configOptions": testOptions("model-a")}
	d, launcher, root := matrixDriver(t,
		scriptScenario{newResult: open, newUpdates: []any{update(envelope)}},
		scriptScenario{loadResult: open, replayUpdates: []any{update(envelope)}},
	)
	created, err := d.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	page, err := created.History(context.Background(), provider.HistoryRequest{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("create history = %+v, %v", page, err)
	}
	ref, _ := provider.NewNativeSessionRef("native-exact")
	resumed, err := d.Resume(context.Background(), provider.ResumeRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root, NativeSession: ref})
	if err != nil {
		t.Fatal(err)
	}
	page, err = resumed.History(context.Background(), provider.HistoryRequest{})
	if err != nil || len(page.Items) != 1 || page.Items[0].TurnID != turnID {
		t.Fatalf("resume history = %+v, %v", page, err)
	}
	state, err := resumed.Reconcile(context.Background(), provider.TurnReference{TurnID: turnID})
	if err != nil || state != provider.TurnAccepted {
		t.Fatalf("reconcile = %s, %v", state, err)
	}
	launcher.mu.Lock()
	first, second := launcher.children[0], launcher.children[1]
	launcher.mu.Unlock()
	if got, want := string(matrixRequest(t, first, "session/new")), `{"cwd":"`+root+`","mcpServers":[]}`; got != want {
		t.Fatalf("new params = %s, want %s", got, want)
	}
	if got, want := string(matrixRequest(t, second, "session/load")), `{"cwd":"`+root+`","mcpServers":[],"sessionId":"native-exact"}`; got != want {
		t.Fatalf("load params = %s, want %s", got, want)
	}
	_ = created.Shutdown(context.Background())
	_ = resumed.Shutdown(context.Background())
}

func TestCursorResumeAcceptsStandardLoadResponseWithoutSessionID(t *testing.T) {
	loaded := map[string]any{"configOptions": testOptions("model-a"), "models": map[string]any{}, "modes": map[string]any{}}
	d, launcher, root := matrixDriver(t, scriptScenario{loadResult: loaded})
	launcher.listPages = map[string]scriptedListPage{
		"": {result: map[string]any{"sessions": []any{map[string]any{"sessionId": "native-standard"}}}},
	}
	ref, _ := provider.NewNativeSessionRef("native-standard")
	opened, err := d.Resume(context.Background(), provider.ResumeRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root, NativeSession: ref})
	if err != nil {
		t.Fatal(err)
	}
	if got := opened.NativeSession().Ref; got != ref {
		t.Fatalf("native ref = %q, want %q", got.Value(), ref.Value())
	}
	_ = opened.Shutdown(context.Background())
}

func TestCursorResumeRejectsMismatchedLoadResponseSessionID(t *testing.T) {
	loaded := map[string]any{"sessionId": "native-other", "configOptions": testOptions("model-a")}
	d, launcher, root := matrixDriver(t, scriptScenario{loadResult: loaded})
	launcher.listPages = map[string]scriptedListPage{
		"": {result: map[string]any{"sessions": []any{map[string]any{"sessionId": "native-requested"}}}},
	}
	ref, _ := provider.NewNativeSessionRef("native-requested")
	opened, err := d.Resume(context.Background(), provider.ResumeRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root, NativeSession: ref})
	assertMatrixProviderError(t, err, provider.ErrorProtocolIncompatible)
	if opened != nil {
		_ = opened.Shutdown(context.Background())
	}
}

func TestCursorResumeProvesNativeSessionExistsBeforeLoad(t *testing.T) {
	open := map[string]any{"sessionId": "missing-native", "configOptions": testOptions("model-a")}
	d, launcher, root := matrixDriver(t, scriptScenario{loadResult: open})
	launcher.listPages = map[string]scriptedListPage{
		"": {result: map[string]any{"sessions": []any{}}},
	}
	ref, _ := provider.NewNativeSessionRef("missing-native")
	opened, err := d.Resume(context.Background(), provider.ResumeRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root, NativeSession: ref})
	assertMatrixProviderError(t, err, provider.ErrorNativeSessionMissing)
	if opened != nil {
		_ = opened.Shutdown(context.Background())
	}
	launcher.mu.Lock()
	child := launcher.children[0]
	launcher.mu.Unlock()
	child.mu.Lock()
	methods := append([]string(nil), child.methods...)
	child.mu.Unlock()
	if !slices.Equal(methods, []string{"initialize", "session/list"}) {
		t.Fatalf("methods = %v", methods)
	}
}

func TestCursorMatrixReplayRejectsKnownInboundRequests(t *testing.T) {
	open := map[string]any{"sessionId": "native", "configOptions": testOptions("model-a")}
	permission := map[string]any{"sessionId": "native", "toolCall": map[string]any{"toolCallId": "tool", "title": "Read", "kind": "read"}, "options": []any{map[string]any{"optionId": "yes", "name": "Allow", "kind": "allow_once"}}}
	updateRequest := map[string]any{"sessionId": "native", "update": map[string]any{"sessionUpdate": "future_standard_update"}}
	for _, tc := range []struct {
		name, method string
		params       any
		wantError    bool
		wantCode     int
	}{
		{name: "permission", method: "session/request_permission", params: permission, wantError: true},
		{name: "update as request", method: "session/update", params: updateRequest, wantError: true, wantCode: -32600},
		{name: "unknown extension", method: "cursor/future_extension", params: map[string]any{}, wantCode: -32601},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, launcher, root := matrixDriver(t, scriptScenario{loadResult: open, loadRequestMethod: tc.method, loadRequestParams: tc.params})
			ref, _ := provider.NewNativeSessionRef("native")
			opened, err := d.Resume(context.Background(), provider.ResumeRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root, NativeSession: ref})
			if tc.wantError {
				assertMatrixProviderError(t, err, provider.ErrorMalformedStream)
			} else if err != nil {
				t.Fatal(err)
			}
			launcher.mu.Lock()
			child := launcher.children[0]
			launcher.mu.Unlock()
			child.mu.Lock()
			responses := append([]json.RawMessage(nil), child.inboundResponses...)
			child.mu.Unlock()
			if len(responses) != 1 {
				t.Fatalf("inbound responses = %d", len(responses))
			}
			if tc.wantCode != 0 {
				var response struct {
					Error struct {
						Code int `json:"code"`
					} `json:"error"`
				}
				if json.Unmarshal(responses[0], &response) != nil || response.Error.Code != tc.wantCode {
					t.Fatalf("response = %s", responses[0])
				}
			} else if !bytes.Contains(responses[0], []byte(`"outcome":"cancelled"`)) {
				t.Fatalf("permission response = %s", responses[0])
			}
			if opened != nil {
				_ = opened.Shutdown(context.Background())
			}
		})
	}
}

func TestCursorMatrixReplayConfigUpdateValidationAndAuthority(t *testing.T) {
	open := map[string]any{"sessionId": "native", "configOptions": testOptions("model-a")}
	ref, _ := provider.NewNativeSessionRef("native")
	t.Run("malformed fails replay", func(t *testing.T) {
		malformed := map[string]any{"sessionId": "native", "update": map[string]any{"sessionUpdate": "config_option_update", "configOptions": []any{map[string]any{"id": "model"}}}}
		d, _, root := matrixDriver(t, scriptScenario{loadResult: open, replayUpdates: []any{malformed}})
		opened, err := d.Resume(context.Background(), provider.ResumeRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root, NativeSession: ref})
		assertMatrixProviderError(t, err, provider.ErrorMalformedStream)
		if opened != nil {
			_ = opened.Shutdown(context.Background())
		}
	})
	t.Run("valid update is discarded in favor of load response", func(t *testing.T) {
		valid := map[string]any{"sessionId": "native", "update": map[string]any{"sessionUpdate": "config_option_update", "configOptions": testOptions("model-b")}}
		d, _, root := matrixDriver(t, scriptScenario{loadResult: open, replayUpdates: []any{valid}})
		opened, err := d.Resume(context.Background(), provider.ResumeRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root, NativeSession: ref})
		if err != nil {
			t.Fatal(err)
		}
		if opened.Model() != "model-a" {
			t.Fatalf("load response was not authoritative: %q", opened.Model())
		}
		_ = opened.Shutdown(context.Background())
	})
}

func assertMatrixProviderError(t *testing.T, err error, code provider.ProviderErrorCode) {
	t.Helper()
	var providerErr provider.ProviderError
	if !errors.As(err, &providerErr) || providerErr.Code() != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
}

func TestCursorMatrixCanonicalSequentialPrompts(t *testing.T) {
	d, launcher, root := matrixDriver(t, scriptScenario{})
	opened, err := d.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
	base := provider.PageContext{Source: []byte("source"), CreatorContext: []byte("creator"), Title: "Board", URL: "https://example.test", Resource: provider.Resource{Kind: provider.ResourceMarkdown, ID: "QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFB", CreatedAt: at, UpdatedAt: at}}
	requests := []provider.TurnRequest{
		{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("initial"), Context: copyMatrixContext(base, provider.ContextInitial)},
		{TurnID: "QUJDREVGR0hJSktMTU5PUFFSU1RVVldY", MessageID: "MTIzNDU2Nzg5MGFiY2RlZjEyMzQ1Njc4", Content: provider.TextMessage("replacement"), Context: copyMatrixContext(base, provider.ContextReplacement)},
		{TurnID: "RkVEREJBOTg3NjU0MzIxMEFCQ0RFRkdI", MessageID: "RkVEREJBOTg3NjU0MzIxMEFCQ0RFRkdJ", Content: provider.TextMessage("continuation")},
	}
	for _, request := range requests {
		if _, err = opened.Submit(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	launcher.mu.Lock()
	child := launcher.children[0]
	launcher.mu.Unlock()
	child.mu.Lock()
	captured := append([]json.RawMessage(nil), child.promptParams...)
	child.mu.Unlock()
	if len(captured) != len(requests) {
		t.Fatalf("captured prompts = %d", len(captured))
	}
	for i, request := range requests {
		want, buildErr := provider.Build(request, provider.PolicyConfigured)
		if buildErr != nil {
			t.Fatal(buildErr)
		}
		var params struct {
			SessionID string `json:"sessionId"`
			Prompt    []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"prompt"`
		}
		if err = json.Unmarshal(captured[i], &params); err != nil {
			t.Fatal(err)
		}
		if params.SessionID == "" || len(params.Prompt) != 1 || params.Prompt[0].Type != "text" || params.Prompt[0].Text != string(want) {
			t.Fatalf("prompt %d mismatch: %+v", i, params)
		}
	}
	_ = opened.Shutdown(context.Background())
}

func copyMatrixContext(base provider.PageContext, revision provider.ContextRevision) *provider.PageContext {
	out := base
	out.Revision = revision
	out.Digest = agent.CalculateContextDigest(out.Source, out.CreatorContext)
	return &out
}

func TestCursorMatrixImageBlockAndNoNativeLeak(t *testing.T) {
	d, launcher, root := matrixDriver(t, scriptScenario{})
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	opened, err := d.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	image := []byte{0x89, 'P', 'N', 'G'}
	path := filepath.Join(root, "input.png")
	if err = os.WriteFile(path, image, 0o600); err != nil {
		t.Fatal(err)
	}
	request := provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("see image"), Images: []provider.ImageInput{{ID: "QUJDREVGR0hJSktMTU5PUFFSU1RVVldY", Name: "input.png", MediaType: "image/png", Bytes: int64(len(image)), Path: path}}}
	if _, err = opened.Submit(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	launcher.mu.Lock()
	child := launcher.children[0]
	launcher.mu.Unlock()
	raw := matrixRequest(t, child, "session/prompt")
	if bytes.Contains(raw, []byte(path)) || bytes.Contains(raw, []byte(request.Images[0].ID)) {
		t.Fatalf("prompt leaked image path/native id: %s", raw)
	}
	var params struct {
		Prompt []map[string]any `json:"prompt"`
	}
	if err = json.Unmarshal(raw, &params); err != nil {
		t.Fatal(err)
	}
	if len(params.Prompt) != 2 || params.Prompt[1]["type"] != "image" || params.Prompt[1]["mimeType"] != "image/png" || params.Prompt[1]["data"] != base64.StdEncoding.EncodeToString(image) {
		t.Fatalf("image block = %+v", params.Prompt)
	}
	_ = opened.Shutdown(context.Background())
}

func TestCursorMatrixThoughtAcceptsWithoutLeakAndGenericRejection(t *testing.T) {
	d, launcher, root := matrixDriver(t, scriptScenario{})
	opened, err := d.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	s := opened.(*Session)
	launcher.mu.Lock()
	child := launcher.children[0]
	launcher.mu.Unlock()
	child.mu.Lock()
	child.promptStarted = make(chan struct{})
	child.promptRelease = make(chan string, 1)
	started, release := child.promptStarted, child.promptRelease
	child.mu.Unlock()
	result := make(chan error, 1)
	go func() {
		_, submitErr := s.Submit(context.Background(), provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("reader")})
		result <- submitErr
	}()
	<-started
	child.send(t, map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": s.NativeSession().Ref.Value(), "update": map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "private"}}}})
	if err = <-result; err != nil {
		t.Fatal(err)
	}
	if event := <-s.Events(); event.Kind != provider.EventUserMessage {
		t.Fatalf("first event = %s", event.Kind)
	}
	select {
	case event := <-s.Events():
		t.Fatalf("thought leaked as %s", event.Kind)
	default:
	}
	release <- "end_turn"
	if event := <-s.Events(); event.Kind != provider.EventCompletion {
		t.Fatalf("terminal = %s", event.Kind)
	}
	_ = s.Shutdown(context.Background())

	d2, _, root2 := matrixDriver(t, scriptScenario{prompts: []scriptedPrompt{{rpcError: &acp.RPCError{Code: -32099, Message: "native rejection"}}}})
	opened2, err := d2.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root2})
	if err != nil {
		t.Fatal(err)
	}
	_, err = opened2.Submit(context.Background(), provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("reader")})
	var pe provider.ProviderError
	if !errors.As(err, &pe) || pe.Code() != provider.ErrorProtocolFailure || pe.Code() == provider.ErrorContextTooLarge {
		t.Fatalf("generic rejection = %v", err)
	}
	_ = opened2.Shutdown(context.Background())
}
