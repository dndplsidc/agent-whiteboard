package cursor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/acp"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	"github.com/dndplsidc/agent-whiteboard/internal/common"
)

type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

type fixedIDs struct{}

func (fixedIDs) NewID() (string, error) { return "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4", nil }

type scriptedListPage struct {
	result   any
	rpcError *acp.RPCError
}

type scriptedPrompt struct {
	result   any
	rpcError *acp.RPCError
}

type scriptScenario struct {
	initializeResult  any
	initializeError   *acp.RPCError
	newResult         any
	newUpdates        []any
	loadResult        any
	replayUpdates     []any
	loadRequestMethod string
	loadRequestParams any
	prompts           []scriptedPrompt
}

type recordedRequest struct {
	method string
	params json.RawMessage
}

type scriptLauncher struct {
	mu           sync.Mutex
	children     []*scriptChild
	requests     []common.ProcessRequest
	earlyInbound bool
	listPages    map[string]scriptedListPage
	scenarios    []scriptScenario
}

func (l *scriptLauncher) Launch(_ context.Context, r common.ProcessRequest) (common.ManagedProcess, error) {
	l.mu.Lock()
	earlyInbound := l.earlyInbound
	var scenario scriptScenario
	if len(l.children) < len(l.scenarios) {
		scenario = cloneScenario(l.scenarios[len(l.children)])
	}
	listPages := make(map[string]scriptedListPage, len(l.listPages))
	for cursor, page := range l.listPages {
		listPages[cursor] = page
	}
	l.mu.Unlock()
	c := newScriptChildWithEarlyInbound(earlyInbound)
	c.listPages = listPages
	c.scenario = scenario
	l.mu.Lock()
	l.children = append(l.children, c)
	l.requests = append(l.requests, r)
	l.mu.Unlock()
	return c, nil
}

func cloneScenario(in scriptScenario) scriptScenario {
	out := in
	out.newUpdates = append([]any(nil), in.newUpdates...)
	out.replayUpdates = append([]any(nil), in.replayUpdates...)
	out.prompts = append([]scriptedPrompt(nil), in.prompts...)
	return out
}

type scriptChild struct {
	inputW            *io.PipeWriter
	inputR            *io.PipeReader
	outputR           *io.PipeReader
	outputW           *io.PipeWriter
	errR              *io.PipeReader
	errW              *io.PipeWriter
	done              chan struct{}
	once              sync.Once
	mu                sync.Mutex
	methods           []string
	observed          []string
	requests          []recordedRequest
	promptParams      []json.RawMessage
	scenario          scriptScenario
	promptIndex       int
	listParams        []json.RawMessage
	listPages         map[string]scriptedListPage
	responses         chan json.RawMessage
	inboundResponses  []json.RawMessage
	setConfigResult   []configOption
	promptStarted     chan struct{}
	promptRelease     chan string
	earlyInbound      bool
	earlyResponse     chan struct{}
	earlyResponseOnce sync.Once
	inputGate         <-chan struct{}
	inputStarted      chan struct{}
}

func newScriptChild() *scriptChild { return newScriptChildWithEarlyInbound(false) }

func newScriptChildWithEarlyInbound(earlyInbound bool) *scriptChild {
	ir, iw := io.Pipe()
	or, ow := io.Pipe()
	er, ew := io.Pipe()
	c := &scriptChild{inputW: iw, inputR: ir, outputR: or, outputW: ow, errR: er, errW: ew, done: make(chan struct{}), responses: make(chan json.RawMessage, 16), earlyInbound: earlyInbound}
	if earlyInbound {
		c.earlyResponse = make(chan struct{})
	}
	go c.serve()
	return c
}

type scriptInput struct{ child *scriptChild }

func (w *scriptInput) Write(p []byte) (int, error) {
	w.child.mu.Lock()
	gate, started := w.child.inputGate, w.child.inputStarted
	if started != nil {
		w.child.inputStarted = nil
	}
	w.child.mu.Unlock()
	if started != nil {
		close(started)
	}
	if gate != nil {
		<-gate
	}
	return w.child.inputW.Write(p)
}
func (w *scriptInput) Close() error { return w.child.inputW.Close() }

func (c *scriptChild) Input() io.WriteCloser { return &scriptInput{child: c} }
func (c *scriptChild) Output() io.Reader     { return c.outputR }
func (c *scriptChild) Errors() io.Reader     { return c.errR }
func (c *scriptChild) Wait() error           { <-c.done; return nil }
func (c *scriptChild) Terminate() error      { c.stop(); return nil }
func (c *scriptChild) Kill() error           { c.stop(); return nil }
func (c *scriptChild) stop() {
	c.once.Do(func() { _ = c.inputR.Close(); _ = c.outputW.Close(); _ = c.errW.Close(); close(c.done) })
}
func (c *scriptChild) send(t *testing.T, value any) {
	t.Helper()
	frame, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.outputW.Write(append(frame, '\n')); err != nil {
		t.Fatal(err)
	}
}
func (c *scriptChild) serve() {
	defer c.stop()
	scan := bufio.NewScanner(c.inputR)
	for scan.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scan.Bytes(), &req) != nil {
			continue
		}
		if req.Method == "" {
			response := append(json.RawMessage(nil), scan.Bytes()...)
			c.mu.Lock()
			c.observed = append(c.observed, "response")
			c.inboundResponses = append(c.inboundResponses, response)
			c.mu.Unlock()
			c.responses <- response
			if c.earlyInbound {
				c.earlyResponseOnce.Do(func() { close(c.earlyResponse) })
			}
			continue
		}
		c.mu.Lock()
		c.methods = append(c.methods, req.Method)
		c.observed = append(c.observed, req.Method)
		c.requests = append(c.requests, recordedRequest{method: req.Method, params: append(json.RawMessage(nil), req.Params...)})
		index := len(c.methods)
		c.mu.Unlock()
		if len(req.ID) == 0 {
			continue
		}
		var result any
		var responseError *acp.RPCError
		switch req.Method {
		case "initialize":
			result = c.scenario.initializeResult
			responseError = c.scenario.initializeError
			if result == nil && responseError == nil {
				result = map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": true, "promptCapabilities": map[string]any{"image": true, "embeddedContext": false}, "sessionCapabilities": map[string]any{"list": map[string]any{}}}}
			}
		case "session/list":
			var params struct {
				Cursor string `json:"cursor"`
			}
			_ = json.Unmarshal(req.Params, &params)
			c.mu.Lock()
			c.listParams = append(c.listParams, append(json.RawMessage(nil), req.Params...))
			page, configured := c.listPages[params.Cursor]
			c.mu.Unlock()
			if configured {
				result, responseError = page.result, page.rpcError
			} else {
				result = map[string]any{"sessions": []any{}}
			}
		case "session/new":
			result = c.scenario.newResult
			if result == nil {
				result = map[string]any{"sessionId": "native-session-" + string(rune('0'+index)), "configOptions": testOptions("model-a")}
			}
			for _, update := range c.scenario.newUpdates {
				notification, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": update})
				_, _ = c.outputW.Write(append(notification, '\n'))
			}
		case "session/load":
			result = c.scenario.loadResult
			if result == nil {
				result = map[string]any{"sessionId": "native", "configOptions": testOptions("model-a")}
			}
			for _, update := range c.scenario.replayUpdates {
				notification, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": update})
				_, _ = c.outputW.Write(append(notification, '\n'))
			}
		case "session/prompt":
			c.mu.Lock()
			c.promptParams = append(c.promptParams, append(json.RawMessage(nil), req.Params...))
			promptIndex := c.promptIndex
			c.promptIndex++
			var scripted scriptedPrompt
			if promptIndex < len(c.scenario.prompts) {
				scripted = c.scenario.prompts[promptIndex]
			}
			c.mu.Unlock()
			c.mu.Lock()
			started := c.promptStarted
			release := c.promptRelease
			c.mu.Unlock()
			if started != nil {
				close(started)
			}
			if release != nil {
				result = map[string]any{"stopReason": <-release}
			} else if scripted.result != nil || scripted.rpcError != nil {
				result, responseError = scripted.result, scripted.rpcError
			} else {
				result = map[string]any{"stopReason": "end_turn"}
			}
		case "session/set_config_option":
			c.mu.Lock()
			configured := append([]configOption(nil), c.setConfigResult...)
			c.mu.Unlock()
			if configured == nil {
				configured = testOptions("model-a")
			}
			result = map[string]any{"configOptions": configured}
		case "session/cancel":
			continue
		default:
			result = map[string]any{}
		}
		response := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if responseError != nil {
			response["error"] = responseError
		} else {
			response["result"] = result
		}
		frame, _ := json.Marshal(response)
		if req.Method == "session/load" && c.scenario.loadRequestMethod != "" {
			inbound, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 9101, "method": c.scenario.loadRequestMethod, "params": c.scenario.loadRequestParams})
			_, _ = c.outputW.Write(append(inbound, '\n'))
			go func() {
				<-c.responses
				_, _ = c.outputW.Write(append(frame, '\n'))
			}()
			continue
		}
		if req.Method == "initialize" && c.earlyInbound {
			early, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 9001, "method": "cursor/early", "params": map[string]any{}})
			_, _ = c.outputW.Write(append(early, '\n'))
			go func() {
				<-c.earlyResponse
				_, _ = c.outputW.Write(append(frame, '\n'))
			}()
			continue
		}
		_, _ = c.outputW.Write(append(frame, '\n'))
	}
}
func testOptions(current string) []configOption {
	return []configOption{{ID: "model", Name: "Model", Description: "Cursor model", Category: "model", Type: "select", CurrentValue: current, Options: []configValue{{Value: "model-a", Name: "Model A"}, {Value: "model-b", Name: "Model B"}}}}
}

func testDriver(t *testing.T) (*Driver, *scriptLauncher, string) {
	t.Helper()
	root := t.TempDir()
	exe := filepath.Join(root, "cursor-agent")
	if err := os.WriteFile(exe, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	launcher := &scriptLauncher{}
	driver, err := NewDriver(Config{Executable: exe, Environment: []string{"HOME=" + root}, ProviderRoot: root, Launcher: launcher, IDs: fixedIDs{}, Clock: fixedClock{time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)}, IdleTimeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return driver, launcher, root
}
func TestReadinessRejectsSymlinkedExecutableAndUnsafeMode(t *testing.T) {
	d, _, root := testDriver(t)
	executable := d.config.Executable
	if err := os.Remove(executable); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real-cursor-agent"), executable); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real-cursor-agent"), []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := d.Readiness(context.Background()); got.State != provider.MissingExecutable {
		t.Fatalf("symlinked executable readiness = %+v", got)
	}

	d, _, _ = testDriver(t)
	if err := os.Chmod(d.config.Executable, 0o702); err != nil {
		t.Fatal(err)
	}
	if got := d.Readiness(context.Background()); got.State != provider.MissingExecutable {
		t.Fatalf("group/other writable executable readiness = %+v", got)
	}
}

func TestReadinessRejectsMultiplyLinkedExecutable(t *testing.T) {
	d, _, root := testDriver(t)
	if err := os.Link(d.config.Executable, filepath.Join(root, "cursor-agent-alias")); err != nil {
		t.Fatal(err)
	}
	if got := d.Readiness(context.Background()); got.State != provider.MissingExecutable {
		t.Fatalf("multiply linked executable readiness = %+v", got)
	}
}

func TestReadinessRejectsWrongOwnerExecutable(t *testing.T) {
	d, _, _ := testDriver(t)
	wrongOwner := os.Geteuid() + 1
	if wrongOwner > 1<<31-1 {
		t.Skip("no representable alternate uid")
	}
	if err := os.Chown(d.config.Executable, wrongOwner, -1); err != nil {
		t.Skipf("chown unavailable: %v", err)
	}
	if got := d.Readiness(context.Background()); got.State != provider.MissingExecutable {
		t.Fatalf("wrong-owner executable readiness = %+v", got)
	}
}

func TestConversationRejectsEarlyInboundRequestBeforePublication(t *testing.T) {
	d, launcher, root := testDriver(t)
	launcher.mu.Lock()
	launcher.earlyInbound = true
	launcher.mu.Unlock()
	opened, err := d.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	launcher.mu.Lock()
	child := launcher.children[0]
	launcher.mu.Unlock()
	select {
	case raw := <-child.responses:
		var response struct {
			Error struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &response); err != nil {
			t.Fatal(err)
		}
		if response.Error.Code != -32601 {
			t.Fatalf("early inbound error code = %d", response.Error.Code)
		}
	case <-time.After(time.Second):
		t.Fatal("early inbound request was not answered")
	}
	_ = opened.Shutdown(context.Background())
}

func TestInboundRouterHandlesEarlyNotificationsAndPublication(t *testing.T) {
	router := &inboundRouter{}
	// A pre-publication notification must be ignored without dereferencing a
	// nil session. Requests take the same path and return method-not-found when
	// they carry a responder (the ACP integration exercises that wire shape).
	router.handle(context.Background(), acp.Request{Method: "session/update"})
	session := &Session{}
	router.publish(session)
	var workers sync.WaitGroup
	for index := 0; index < 16; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			router.handle(context.Background(), acp.Request{Method: "session/update"})
		}()
	}
	workers.Wait()
	session.mu.Lock()
	bad := session.replayBad
	session.mu.Unlock()
	if !bad {
		t.Fatal("published router did not deliver notification")
	}
}

func TestReadinessProbeIsNonMutating(t *testing.T) {
	d, l, _ := testDriver(t)
	ready := d.Readiness(context.Background())
	if ready.State != provider.Ready {
		t.Fatalf("%+v", ready)
	}
	l.mu.Lock()
	methods := append([]string(nil), l.children[0].methods...)
	l.mu.Unlock()
	for _, method := range methods {
		if method != "initialize" && method != "session/list" {
			t.Fatalf("mutating readiness method %q", method)
		}
	}
}
func TestEachConversationOwnsDistinctChildAndWorkspace(t *testing.T) {
	d, l, root := testDriver(t)
	w1 := filepath.Join(root, "one")
	w2 := filepath.Join(root, "two")
	_ = os.Mkdir(w1, 0700)
	_ = os.Mkdir(w2, 0700)
	s1, err := d.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: w1})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := d.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: w2})
	if err != nil {
		t.Fatal(err)
	}
	if s1.Child() == s2.Child() {
		t.Fatal("conversations shared child")
	}
	l.mu.Lock()
	if l.requests[0].WorkingDirectory != w1 || l.requests[1].WorkingDirectory != w2 {
		t.Fatal("workspace cwd not isolated")
	}
	l.mu.Unlock()
	_ = s1.Shutdown(context.Background())
	_ = s2.Shutdown(context.Background())
}
func TestCatalogUsesExactNativeValuesAndSingletonProjection(t *testing.T) {
	catalog, settings, presentation, err := catalogFromOptions(testOptions("model-b"), true)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Model != "model-b" || settings.Effort != "default" || settings.Speed != provider.SpeedStandard || presentation.ModelDisplayName != "Model B" || !catalog.Models[1].SupportsImages {
		t.Fatalf("%+v %+v", settings, presentation)
	}
}

func TestPromptAdmissionPublishesUserBeforeBufferedAssistant(t *testing.T) {
	driver, launcher, root := testDriver(t)
	opened, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	session := opened.(*Session)
	launcher.mu.Lock()
	child := launcher.children[0]
	launcher.mu.Unlock()
	child.mu.Lock()
	child.promptStarted = make(chan struct{})
	child.promptRelease = make(chan string, 1)
	started, release := child.promptStarted, child.promptRelease
	child.mu.Unlock()
	request := provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("reader")}
	accepted := make(chan error, 1)
	go func() {
		_, submitErr := session.Submit(context.Background(), request)
		accepted <- submitErr
	}()
	<-started
	nativeSession := session.NativeSession().Ref.Value()
	child.send(t, map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": nativeSession, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "answer"}}}})
	if err = <-accepted; err != nil {
		t.Fatal(err)
	}
	first, second := <-session.events, <-session.events
	if first.Kind != provider.EventUserMessage || second.Kind != provider.EventAssistantDelta {
		t.Fatalf("event order = %s, %s", first.Kind, second.Kind)
	}
	release <- "end_turn"
	if event := <-session.events; event.Kind != provider.EventAssistantMessage {
		t.Fatalf("final event = %s", event.Kind)
	}
	if event := <-session.events; event.Kind != provider.EventCompletion {
		t.Fatalf("terminal event = %s", event.Kind)
	}
	_ = session.Shutdown(context.Background())
}

func TestMalformedPreAdmissionUpdateRejectsAndQuarantinesPrompt(t *testing.T) {
	driver, launcher, root := testDriver(t)
	opened, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	session := opened.(*Session)
	launcher.mu.Lock()
	child := launcher.children[0]
	launcher.mu.Unlock()
	child.mu.Lock()
	child.promptStarted = make(chan struct{})
	child.promptRelease = make(chan string, 1)
	started, release := child.promptStarted, child.promptRelease
	child.mu.Unlock()
	request := provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("reader")}
	result := make(chan error, 1)
	go func() { _, submitErr := session.Submit(context.Background(), request); result <- submitErr }()
	<-started
	nativeSession := session.NativeSession().Ref.Value()
	child.send(t, map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": nativeSession, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": ""}}}})
	if err = <-result; err == nil {
		t.Fatal("malformed update did not reject Submit")
	}
	child.send(t, map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": nativeSession, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "late"}}}})
	release <- "end_turn"
	select {
	case event, ok := <-session.events:
		if ok {
			t.Fatalf("quarantined prompt emitted %s", event.Kind)
		}
	default:
	}
	_ = session.Shutdown(context.Background())
}

func TestSubmitRejectsSettingsDriftBeforeReadingImages(t *testing.T) {
	driver, _, root := testDriver(t)
	opened, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	session := opened.(*Session)
	settings := provider.ExecutionSettings{Model: "model-b", Effort: "default", Speed: provider.SpeedStandard}
	request := provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("reader"), Settings: &settings, Images: []provider.ImageInput{{ID: "QUJDREVGR0hJSktMTU5PUFFSU1RVVldY", Name: "missing.png", MediaType: "image/png", Bytes: 1, Path: filepath.Join(root, "missing.png")}}}
	_, err = session.Submit(context.Background(), request)
	var providerErr provider.ProviderError
	if !errors.As(err, &providerErr) || providerErr.Code() != provider.ErrorInvalidModelConfiguration {
		t.Fatalf("Submit error = %v", err)
	}
	_ = session.Shutdown(context.Background())
}

func TestApplySettingsRefreshesValidAuthoritativeDriftBeforeRejecting(t *testing.T) {
	driver, launcher, root := testDriver(t)
	opened, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	session := opened.(*Session)
	launcher.mu.Lock()
	child := launcher.children[0]
	launcher.mu.Unlock()
	child.mu.Lock()
	child.setConfigResult = testOptions("model-a")
	child.mu.Unlock()
	wanted := provider.ExecutionSettings{Model: "model-b", Effort: "default", Speed: provider.SpeedStandard}
	if _, _, err = session.ApplySettings(context.Background(), wanted); err == nil {
		t.Fatal("drifted returned settings accepted")
	}
	if got := session.Model(); got != "model-a" {
		t.Fatalf("authoritative model not refreshed: %q", got)
	}
	_ = session.Shutdown(context.Background())
}

func TestCatalogRejectsMalformedCompleteConfiguration(t *testing.T) {
	cases := []struct {
		name string
		edit func([]configOption)
	}{
		{name: "missing option name", edit: func(v []configOption) { v[0].Name = "" }},
		{name: "missing type", edit: func(v []configOption) { v[0].Type = "" }},
		{name: "unknown current value", edit: func(v []configOption) { v[0].CurrentValue = "missing" }},
		{name: "duplicate value", edit: func(v []configOption) { v[0].Options[1].Value = v[0].Options[0].Value }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			options := testOptions("model-a")
			tc.edit(options)
			if _, _, _, err := catalogFromOptions(options, true); err == nil {
				t.Fatal("malformed configuration accepted")
			}
		})
	}
}
