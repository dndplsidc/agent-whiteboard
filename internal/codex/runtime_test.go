package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/contentturn"
	"github.com/edocsss/agent-whiteboard/internal/contextdigest"
	"github.com/edocsss/agent-whiteboard/internal/provider"
	"github.com/stretchr/testify/require"
)

func TestDriverUsesDefaultConfigurationAndRelaysActivityAndApproval(t *testing.T) {
	serverResponses := make(chan map[string]json.RawMessage, 1)
	launcher := &scriptedLauncher{serve: func(child *scriptedChild) {
		scanner := bufio.NewScanner(child.serverInput)
		for scanner.Scan() {
			var request map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(scanner.Bytes(), &request))
			var method string
			_ = json.Unmarshal(request["method"], &method)
			switch method {
			case "initialize":
				child.send(t, map[string]any{"id": request["id"], "result": map[string]any{
					"codexHome": "/fixture/home", "platformFamily": "unix", "platformOs": "linux", "userAgent": "fixture",
				}})
			case "initialized":
			case "account/read":
				child.send(t, map[string]any{"id": request["id"], "result": map[string]any{"account": map[string]any{"type": "apiKey"}, "requiresOpenaiAuth": true}})
			case "thread/start":
				var params map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(request["params"], &params))
				require.Equal(t, []string{"cwd"}, sortedKeys(params))
				child.send(t, map[string]any{"id": request["id"], "result": map[string]any{"thread": map[string]any{"id": "native-thread"}, "model": "gpt-fixture"}})
			case "turn/start":
				var params struct {
					ThreadID string `json:"threadId"`
					Input    []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"input"`
				}
				require.NoError(t, json.Unmarshal(request["params"], &params))
				require.Equal(t, "native-thread", params.ThreadID)
				require.Len(t, params.Input, 1)
				envelope, err := contentturn.Parse([]byte(params.Input[0].Text))
				require.NoError(t, err)
				require.Equal(t, contentturn.PolicyConfigured, envelope.Policy)
				require.Equal(t, "# Whiteboard", string(envelope.Markdown))
				require.Equal(t, "What changed?", envelope.ReaderMessage)
				// Exercise notifications that arrive before turn/start is acknowledged.
				child.send(t, notification("item/agentMessage/delta", map[string]any{"threadId": "native-thread", "turnId": "native-turn", "delta": "Working"}))
				child.send(t, map[string]any{"id": 700, "method": "item/commandExecution/requestApproval", "params": map[string]any{"threadId": "native-thread", "turnId": "native-turn", "itemId": "native-item", "startedAtMs": 1, "command": "go test ./...", "cwd": "/workspace", "reason": "Run the tests"}})
				child.send(t, map[string]any{"id": request["id"], "result": map[string]any{"turn": map[string]any{"id": "native-turn"}}})
				child.send(t, notification("item/started", map[string]any{"threadId": "native-thread", "turnId": "native-turn", "item": map[string]any{"id": "native-item", "type": "commandExecution", "command": "go test ./...", "cwd": "/workspace", "status": "inProgress"}}))
			default:
				if len(request["id"]) != 0 && len(request["result"]) != 0 {
					serverResponses <- request
					child.send(t, notification("item/completed", map[string]any{"threadId": "native-thread", "turnId": "native-turn", "item": map[string]any{"id": "native-item", "type": "commandExecution", "command": "go test ./...", "cwd": "/workspace", "status": "completed", "aggregatedOutput": "ok"}}))
					child.send(t, notification("item/completed", map[string]any{"threadId": "native-thread", "turnId": "native-turn", "item": map[string]any{"id": "native-message", "type": "agentMessage", "text": "Everything passes."}}))
					child.send(t, notification("turn/completed", map[string]any{"threadId": "native-thread", "turn": map[string]any{"id": "native-turn", "status": "completed"}}))
				}
			}
		}
	}}

	root := t.TempDir()
	environment := []string{"HOME=/Users/tester", "PATH=/fixture/bin", "CODEX_HOME=/Users/tester/.codex-custom"}
	driver, err := NewDriver(Config{
		Executable: "/fixture/bin/codex", Environment: environment, ProviderRoot: root,
		Launcher: launcher, IDs: &sequenceIDs{}, Clock: fixedClock{time.Unix(100, 0).UTC()}, IdleTimeout: time.Hour,
	})
	require.NoError(t, err)

	now := time.Unix(50, 0).UTC()
	resourceID := testID(90)
	markdown := []byte("# Whiteboard")
	creator := []byte("creator context")
	session, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCodex, Access: provider.AccessConfigured, Workspace: "/workspace"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Shutdown(context.Background()) })
	request := provider.TurnRequest{
		TurnID: testID(91), MessageID: testID(92), Message: "What changed?",
		Context: &provider.PageContext{Revision: provider.ContextInitial, Markdown: markdown, CreatorContext: creator, Title: "Board", URL: "https://example.com/board", Resource: provider.Resource{Kind: provider.ResourceMarkdown, ID: resourceID, CreatedAt: now, UpdatedAt: now}, Digest: contextdigest.Calculate(markdown, creator)},
	}
	accepted, err := session.Submit(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, request.TurnID, accepted.TurnID)

	var interaction provider.InteractionRequest
	var statuses []provider.ToolStatus
	var eventKinds []provider.EventKind
	for interaction.ID == "" || len(statuses) == 0 {
		select {
		case event := <-session.Events():
			eventKinds = append(eventKinds, event.Kind)
			switch event.Kind {
			case provider.EventInteractionRequest:
				interaction = *event.Interaction
			case provider.EventToolActivity:
				statuses = append(statuses, event.Tool.Status)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for Codex activity")
		}
	}
	require.Equal(t, []provider.EventKind{
		provider.EventUserMessage,
		provider.EventAssistantDelta,
		provider.EventInteractionRequest,
		provider.EventToolActivity,
	}, eventKinds)
	require.Equal(t, provider.InteractionCommandApproval, interaction.Kind)
	require.NoError(t, session.(provider.InteractiveSession).Respond(context.Background(), provider.InteractionResponse{RequestID: interaction.ID, Kind: interaction.Kind, OptionID: "accept"}))

	response := <-serverResponses
	var responseID int
	require.NoError(t, json.Unmarshal(response["id"], &responseID))
	require.Equal(t, 700, responseID)
	require.JSONEq(t, `{"decision":"accept"}`, string(response["result"]))

	completed := false
	for !completed {
		select {
		case event := <-session.Events():
			if event.Kind == provider.EventToolActivity {
				statuses = append(statuses, event.Tool.Status)
			}
			completed = event.Kind == provider.EventCompletion
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for turn completion")
		}
	}
	require.Contains(t, statuses, provider.ToolRunning)
	require.Contains(t, statuses, provider.ToolCompleted)

	require.Len(t, launcher.requests, 1)
	launch := launcher.requests[0]
	require.Equal(t, []string{"app-server"}, launch.Arguments)
	require.Equal(t, environment, launch.Environment)
	require.Equal(t, root, launch.WorkingDirectory)
}

func TestTurnAcceptanceBarrierOrdersPreAckEventBeforeImmediatePostAckInteractionAndCompletion(t *testing.T) {
	clock := &blockingClock{entered: make(chan struct{}), release: make(chan struct{}), now: time.Unix(100, 0).UTC()}
	input := newSignalingWriteCloser()
	runtime := &runtime{input: input, pending: make(map[int64]chan rpcResult), barriers: make(map[int64]*responseBarrier), inbound: make(map[string]struct{}), sessions: make(map[string]*Session), done: make(chan struct{})}
	driver := &Driver{config: Config{Clock: clock, IDs: &sequenceIDs{}, IdleTimeout: time.Hour}, runtime: runtime}
	session := &Session{
		driver: driver, runtime: runtime, threadID: "native-thread", events: make(chan provider.Event, 8), view: newSessionChild(),
		activities: make(map[string]string), toolStates: make(map[string]provider.ToolActivity), interactions: make(map[string]nativeInteraction),
	}
	runtime.sessions[session.threadID] = session
	request := provider.TurnRequest{TurnID: testID(93), MessageID: testID(94), Message: "Order this stream"}
	submitted := make(chan error, 1)
	go func() { _, err := session.Submit(context.Background(), request); submitted <- err }()
	<-input.wrote

	lines := []string{
		string(mustJSON(t, notification("item/agentMessage/delta", map[string]any{"threadId": "native-thread", "turnId": "native-turn", "delta": "pre-ack"}))),
		string(mustJSON(t, map[string]any{"id": 1, "result": map[string]any{"turn": map[string]any{"id": "native-turn"}}})),
		string(mustJSON(t, map[string]any{"id": 700, "method": "item/commandExecution/requestApproval", "params": map[string]any{
			"threadId": "native-thread", "turnId": "native-turn", "itemId": "native-item", "startedAtMs": 1, "command": "go test", "cwd": "/workspace",
		}})),
		string(mustJSON(t, notification("turn/completed", map[string]any{"threadId": "native-thread", "turn": map[string]any{"id": "native-turn", "status": "completed"}}))),
	}
	go runtime.readLoop(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	<-clock.entered
	select {
	case event := <-session.events:
		t.Fatalf("post-ack stream escaped response barrier before acceptance: %#v", event)
	default:
	}
	close(clock.release)
	require.NoError(t, <-submitted)

	kinds := make([]provider.EventKind, 0, 4)
	for len(kinds) < 4 {
		kinds = append(kinds, awaitEvent(t, session.events).Kind)
	}
	require.Equal(t, []provider.EventKind{
		provider.EventUserMessage,
		provider.EventAssistantDelta,
		provider.EventInteractionRequest,
		provider.EventCompletion,
	}, kinds)
}

func TestTurnCompletionBeforeAcceptanceResponseIsBuffered(t *testing.T) {
	input := newSignalingWriteCloser()
	runtime := &runtime{input: input, pending: make(map[int64]chan rpcResult), barriers: make(map[int64]*responseBarrier), inbound: make(map[string]struct{}), sessions: make(map[string]*Session), done: make(chan struct{})}
	driver := &Driver{config: Config{Clock: fixedClock{time.Unix(100, 0).UTC()}, IDs: &sequenceIDs{}, IdleTimeout: time.Hour}, runtime: runtime}
	session := &Session{
		driver: driver, runtime: runtime, threadID: "native-thread", events: make(chan provider.Event, 4), view: newSessionChild(),
		activities: make(map[string]string), toolStates: make(map[string]provider.ToolActivity), interactions: make(map[string]nativeInteraction),
	}
	runtime.sessions[session.threadID] = session
	request := provider.TurnRequest{TurnID: testID(95), MessageID: testID(96), Message: "Complete immediately"}
	submitted := make(chan error, 1)
	go func() { _, err := session.Submit(context.Background(), request); submitted <- err }()
	<-input.wrote

	lines := []string{
		string(mustJSON(t, notification("turn/completed", map[string]any{"threadId": "native-thread", "turn": map[string]any{"id": "native-turn", "status": "completed"}}))),
		string(mustJSON(t, map[string]any{"id": 1, "result": map[string]any{"turn": map[string]any{"id": "native-turn"}}})),
	}
	go runtime.readLoop(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	require.NoError(t, <-submitted)
	require.Equal(t, provider.EventUserMessage, awaitEvent(t, session.events).Kind)
	require.Equal(t, provider.EventCompletion, awaitEvent(t, session.events).Kind)
}

func TestConcurrentRuntimeWaitersShareOneStartupFailure(t *testing.T) {
	launcher := &failingLauncher{started: make(chan struct{}), release: make(chan struct{})}
	driver, err := NewDriver(Config{
		Executable: "/fixture/bin/codex", Environment: []string{"PATH=/fixture/bin"}, ProviderRoot: t.TempDir(),
		Launcher: launcher, IDs: &sequenceIDs{}, Clock: fixedClock{time.Unix(100, 0).UTC()}, IdleTimeout: time.Hour,
	})
	require.NoError(t, err)

	errs := make(chan error, 2)
	go func() { _, callErr := driver.ensureRuntime(context.Background()); errs <- callErr }()
	<-launcher.started
	go func() { _, callErr := driver.ensureRuntime(context.Background()); errs <- callErr }()
	time.Sleep(20 * time.Millisecond)
	close(launcher.release)

	assertProviderError(t, <-errs, provider.ErrorStartupFailed)
	assertProviderError(t, <-errs, provider.ErrorStartupFailed)
	launcher.mu.Lock()
	calls := launcher.calls
	launcher.mu.Unlock()
	require.Equal(t, 1, calls)
}

func TestIdleTimerCannotStopRuntimeDuringSessionActivation(t *testing.T) {
	clock := &blockingClock{entered: make(chan struct{}), release: make(chan struct{}), now: time.Unix(100, 0).UTC()}
	launcher := readyLauncher(t, func(child *scriptedChild, request map[string]json.RawMessage, method string) {
		if method == "thread/start" {
			child.send(t, map[string]any{"id": request["id"], "result": map[string]any{"thread": map[string]any{"id": "native-thread"}, "model": "gpt-fixture"}})
		}
	})
	driver, err := NewDriver(Config{
		Executable: "/fixture/bin/codex", Environment: []string{"PATH=/fixture/bin"}, ProviderRoot: t.TempDir(),
		Launcher: launcher, IDs: &sequenceIDs{}, Clock: clock, IdleTimeout: 10 * time.Millisecond,
	})
	require.NoError(t, err)

	created := make(chan provider.Session, 1)
	createErr := make(chan error, 1)
	go func() {
		session, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCodex, Access: provider.AccessConfigured, Workspace: "/workspace"})
		created <- session
		createErr <- err
	}()
	<-clock.entered
	time.Sleep(30 * time.Millisecond)
	launcher.mu.Lock()
	child := launcher.children[0]
	launcher.mu.Unlock()
	select {
	case <-child.done:
		t.Fatal("idle timer stopped runtime while session activation held a lease")
	default:
	}
	close(clock.release)
	session := <-created
	require.NoError(t, <-createErr)
	require.NoError(t, session.Shutdown(context.Background()))
}

func TestStaleIdleTimerCannotClearOrActOnReplacement(t *testing.T) {
	runtime := &runtime{input: discardWriteCloser{}, pending: make(map[int64]chan rpcResult), inbound: make(map[string]struct{}), sessions: make(map[string]*Session), done: make(chan struct{})}
	driver := &Driver{config: Config{IdleTimeout: time.Hour}, runtime: runtime}

	driver.mu.Lock()
	driver.scheduleIdleLocked(runtime)
	staleToken := driver.idleToken
	driver.stopIdleLocked()
	driver.scheduleIdleLocked(runtime)
	replacement := driver.idleTimer
	driver.mu.Unlock()

	driver.expireIdle(runtime, staleToken)
	driver.mu.Lock()
	require.Same(t, replacement, driver.idleTimer)
	driver.stopIdleLocked()
	driver.mu.Unlock()
	select {
	case <-runtime.done:
		t.Fatal("stale idle callback stopped the runtime")
	default:
	}
}

func TestDriverSharesOneRuntimeAcrossConcurrentThreadsAndStopsAfterLastDetach(t *testing.T) {
	var threadMu sync.Mutex
	threadCount := 0
	launcher := &scriptedLauncher{serve: func(child *scriptedChild) {
		scanner := bufio.NewScanner(child.serverInput)
		for scanner.Scan() {
			var request map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(scanner.Bytes(), &request))
			var method string
			_ = json.Unmarshal(request["method"], &method)
			switch method {
			case "initialize":
				child.send(t, map[string]any{"id": request["id"], "result": map[string]any{
					"codexHome": "/fixture/home", "platformFamily": "unix", "platformOs": "linux", "userAgent": "fixture",
				}})
			case "initialized":
			case "account/read":
				child.send(t, map[string]any{"id": request["id"], "result": map[string]any{"account": map[string]any{"type": "apiKey"}, "requiresOpenaiAuth": true}})
			case "thread/start":
				threadMu.Lock()
				threadCount++
				threadID := "native-thread-" + strconv.Itoa(threadCount)
				threadMu.Unlock()
				child.send(t, map[string]any{"id": request["id"], "result": map[string]any{"thread": map[string]any{"id": threadID}, "model": "gpt-fixture"}})
			}
		}
	}}
	driver, err := NewDriver(Config{
		Executable: "/fixture/bin/codex", Environment: []string{"PATH=/fixture/bin"}, ProviderRoot: t.TempDir(),
		Launcher: launcher, IDs: &sequenceIDs{}, Clock: fixedClock{time.Unix(100, 0).UTC()}, IdleTimeout: 20 * time.Millisecond,
	})
	require.NoError(t, err)

	created := make(chan provider.Session, 2)
	errors := make(chan error, 2)
	for index := 0; index < 2; index++ {
		go func(index int) {
			session, createErr := driver.Create(context.Background(), provider.CreateRequest{
				Provider: provider.NameCodex, Access: provider.AccessConfigured, Workspace: "/workspace-" + strconv.Itoa(index),
			})
			created <- session
			errors <- createErr
		}(index)
	}
	first, second := <-created, <-created
	require.NoError(t, <-errors)
	require.NoError(t, <-errors)
	require.NotNil(t, first)
	require.NotNil(t, second)
	require.Same(t, first.(*Session).runtime, second.(*Session).runtime)
	require.Len(t, launcher.requests, 1)

	require.NoError(t, first.Shutdown(context.Background()))
	select {
	case <-first.(*Session).runtime.done:
		t.Fatal("shared runtime stopped while another session remained attached")
	case <-time.After(50 * time.Millisecond):
	}
	require.NoError(t, second.Shutdown(context.Background()))
	select {
	case <-second.(*Session).runtime.done:
	case <-time.After(time.Second):
		t.Fatal("shared runtime did not stop after the final session detached")
	}
}

func TestDriverResumeReadDeleteInterruptCompactionAndContextOverflow(t *testing.T) {
	launcher := readyLauncher(t, func(child *scriptedChild, request map[string]json.RawMessage, method string) {
		switch method {
		case "thread/resume":
			child.send(t, map[string]any{"id": request["id"], "result": map[string]any{"thread": map[string]any{"id": "native-thread"}, "model": "gpt-fixture"}})
		case "thread/read":
			child.send(t, map[string]any{"id": request["id"], "result": map[string]any{"thread": map[string]any{"id": "native-thread", "turns": []any{}}, "model": "gpt-fixture"}})
		case "thread/delete":
			child.send(t, map[string]any{"id": request["id"], "result": map[string]any{}})
		case "turn/start":
			child.send(t, map[string]any{"id": request["id"], "result": map[string]any{"turn": map[string]any{"id": "native-turn"}}})
		case "turn/interrupt":
			child.send(t, map[string]any{"id": request["id"], "result": map[string]any{}})
			child.send(t, notification("thread/compacted", map[string]any{"threadId": "native-thread", "turnId": "native-turn"}))
			child.send(t, notification("turn/completed", map[string]any{
				"threadId": "native-thread",
				"turn":     map[string]any{"id": "native-turn", "status": "failed", "error": map[string]any{"code": "contextWindowExceeded"}},
			}))
		}
	})
	driver, err := NewDriver(Config{
		Executable: "/fixture/bin/codex", Environment: []string{"PATH=/fixture/bin"}, ProviderRoot: t.TempDir(),
		Launcher: launcher, IDs: &sequenceIDs{}, Clock: fixedClock{time.Unix(100, 0).UTC()}, IdleTimeout: time.Hour,
	})
	require.NoError(t, err)
	ref, err := provider.NewNativeSessionRef("native-thread")
	require.NoError(t, err)

	inspected, err := driver.Inspect(context.Background(), provider.InspectRequest{Provider: provider.NameCodex, NativeSession: ref})
	require.NoError(t, err)
	require.Equal(t, "native-thread", inspected.Ref.Value())
	resumed, err := driver.Resume(context.Background(), provider.ResumeRequest{Provider: provider.NameCodex, Access: provider.AccessConfigured, NativeSession: ref, Workspace: "/workspace"})
	require.NoError(t, err)
	session := resumed.(*Session)
	page, err := session.History(context.Background(), provider.HistoryRequest{})
	require.NoError(t, err)
	require.Empty(t, page.Items)
	state, err := session.Reconcile(context.Background(), provider.TurnReference{TurnID: testID(700)})
	require.NoError(t, err)
	require.Equal(t, provider.TurnNotAccepted, state)

	request := provider.TurnRequest{TurnID: testID(701), MessageID: testID(702), Message: "Continue"}
	accepted, err := session.Submit(context.Background(), request)
	require.NoError(t, err)
	require.NoError(t, session.Interrupt(context.Background(), accepted))
	foundCompaction := false
	for {
		event := awaitEvent(t, session.Events())
		if event.Kind == provider.EventActivity && event.Activity == provider.ActivityCompaction {
			foundCompaction = true
		}
		if event.Kind == provider.EventTerminalFailure {
			require.Equal(t, provider.ErrorContextTooLarge, event.Failure.Code())
			break
		}
	}
	require.True(t, foundCompaction)
	require.NoError(t, session.Shutdown(context.Background()))
	require.NoError(t, driver.Delete(context.Background(), provider.DeleteRequest{Provider: provider.NameCodex, NativeSession: ref}))
}

func TestMissingResumeMapsToNativeSessionMissingAndDeleteIsIdempotent(t *testing.T) {
	launcher := readyLauncher(t, func(child *scriptedChild, request map[string]json.RawMessage, method string) {
		switch method {
		case "thread/resume", "thread/delete":
			child.send(t, map[string]any{"id": request["id"], "error": map[string]any{
				"code": -32000, "message": "thread not found", "data": map[string]any{"code": "threadNotFound"},
			}})
		}
	})
	driver, err := NewDriver(Config{
		Executable: "/fixture/bin/codex", Environment: []string{"PATH=/fixture/bin"}, ProviderRoot: t.TempDir(),
		Launcher: launcher, IDs: &sequenceIDs{}, Clock: fixedClock{time.Unix(100, 0).UTC()}, IdleTimeout: time.Hour,
	})
	require.NoError(t, err)
	ref, err := provider.NewNativeSessionRef("missing-thread")
	require.NoError(t, err)

	_, err = driver.Resume(context.Background(), provider.ResumeRequest{Provider: provider.NameCodex, Access: provider.AccessConfigured, NativeSession: ref, Workspace: "/workspace"})
	assertProviderError(t, err, provider.ErrorNativeSessionMissing)
	require.NoError(t, driver.Delete(context.Background(), provider.DeleteRequest{Provider: provider.NameCodex, NativeSession: ref}))
	driver.mu.Lock()
	runtime := driver.runtime
	driver.stopIdleLocked()
	driver.runtime = nil
	driver.mu.Unlock()
	if runtime != nil {
		runtime.close()
	}
}

func TestInteractionTranslationUsesOpaqueBrowserKeys(t *testing.T) {
	session := &Session{driver: &Driver{config: Config{IDs: &sequenceIDs{}}}, interactions: make(map[string]nativeInteraction), activities: make(map[string]string), active: &nativeTurn{request: provider.TurnRequest{TurnID: testID(9)}, nativeID: "turn"}}
	questions := json.RawMessage(`{"threadId":"thread","turnId":"turn","itemId":"item","questions":[{"id":"native question id","header":"Choose","question":"Which value?","options":[{"label":"value with spaces","description":"desc"}]}]}`)
	request, pending, err := session.normalizeInteraction(testID(1), "item/tool/requestUserInput", questions)
	require.NoError(t, err)
	require.Equal(t, "question0", request.Questions[0].ID)
	require.Equal(t, "option0", request.Questions[0].Options[0].ID)
	result, err := interactionRPCResult(pending, provider.InteractionResponse{RequestID: request.ID, Kind: request.Kind, Answers: map[string][]string{"question0": {"option0"}}})
	require.NoError(t, err)
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	require.JSONEq(t, `{"answers":{"native question id":{"answers":["value with spaces"]}}}`, string(encoded))
}

func TestValidateJSONStructureRejectsDuplicateKeys(t *testing.T) {
	require.NoError(t, validateJSONStructure([]byte(`{"id":1,"result":{"ok":true}}`)))
	require.Error(t, validateJSONStructure([]byte(`{"id":1,"result":{"ok":true,"ok":false}}`)))
	require.Error(t, validateJSONStructure([]byte(`{"id":1} {"id":2}`)))
}

func TestRuntimeResponseIDsDistinguishUnknownLateAndDuplicate(t *testing.T) {
	t.Run("duplicate and late allocated ids are ignored", func(t *testing.T) {
		waiter := make(chan rpcResult, 1)
		runtime := &runtime{
			input: discardWriteCloser{}, nextID: 2, pending: map[int64]chan rpcResult{1: waiter}, barriers: make(map[int64]*responseBarrier),
			inbound: make(map[string]struct{}), sessions: make(map[string]*Session), done: make(chan struct{}),
		}
		runtime.handleResponse(rpcEnvelope{ID: json.RawMessage(`1`), Result: json.RawMessage(`{}`)})
		require.NoError(t, (<-waiter).err)
		runtime.handleResponse(rpcEnvelope{ID: json.RawMessage(`1`), Result: json.RawMessage(`{}`)})
		runtime.handleResponse(rpcEnvelope{ID: json.RawMessage(`2`), Result: json.RawMessage(`{}`)})
		select {
		case <-runtime.done:
			t.Fatal("late or duplicate response stopped runtime")
		default:
		}
	})

	t.Run("never allocated id fails closed", func(t *testing.T) {
		runtime := &runtime{input: discardWriteCloser{}, nextID: 2, pending: make(map[int64]chan rpcResult), barriers: make(map[int64]*responseBarrier), inbound: make(map[string]struct{}), sessions: make(map[string]*Session), done: make(chan struct{})}
		runtime.handleResponse(rpcEnvelope{ID: json.RawMessage(`3`), Result: json.RawMessage(`{}`)})
		select {
		case <-runtime.done:
		case <-time.After(time.Second):
			t.Fatal("unknown response id did not stop runtime")
		}
		assertProviderError(t, runtime.err, provider.ErrorMalformedStream)
	})
}

func TestInteractionRejectsMalformedParamsAndInvalidResponseWithoutConsumingRequest(t *testing.T) {
	driver := &Driver{config: Config{IDs: &sequenceIDs{}}}
	session := &Session{driver: driver, interactions: make(map[string]nativeInteraction), activities: make(map[string]string)}

	_, _, err := session.normalizeInteraction(testID(1), "item/commandExecution/requestApproval", json.RawMessage(`{"threadId":"thread"}`))
	require.Error(t, err)

	serverInput, clientInput := io.Pipe()
	runtime := &runtime{input: clientInput, inbound: make(map[string]struct{})}
	session.runtime = runtime
	requestID := testID(2)
	rpcID := json.RawMessage(`"native-request"`)
	runtime.inbound[requestIDKey(t, rpcID)] = struct{}{}
	session.interactions[requestID] = nativeInteraction{
		rpcID:  rpcID,
		method: "item/commandExecution/requestApproval",
		request: provider.InteractionRequest{
			ID: requestID, Kind: provider.InteractionCommandApproval, Title: "Approve command",
			Options: approvalOptions(),
		},
	}

	require.Error(t, session.Respond(context.Background(), provider.InteractionResponse{
		RequestID: requestID, Kind: provider.InteractionFileApproval, OptionID: "accept",
	}))
	require.Error(t, session.Respond(context.Background(), provider.InteractionResponse{
		RequestID: requestID, Kind: provider.InteractionCommandApproval, OptionID: "other",
	}))

	written := make(chan map[string]json.RawMessage, 1)
	go func() {
		scanner := bufio.NewScanner(serverInput)
		if scanner.Scan() {
			var response map[string]json.RawMessage
			_ = json.Unmarshal(scanner.Bytes(), &response)
			written <- response
		}
	}()
	require.NoError(t, session.Respond(context.Background(), provider.InteractionResponse{
		RequestID: requestID, Kind: provider.InteractionCommandApproval, OptionID: "accept",
	}))
	require.Error(t, session.Respond(context.Background(), provider.InteractionResponse{
		RequestID: requestID, Kind: provider.InteractionCommandApproval, OptionID: "accept",
	}))
	response := <-written
	require.JSONEq(t, `{"decision":"accept"}`, string(response["result"]))
	require.NoError(t, clientInput.Close())
}

func TestInteractionTranslationFailureDoesNotConsumePendingRequest(t *testing.T) {
	serverInput, clientInput := io.Pipe()
	rpcID := json.RawMessage(`"native-request"`)
	runtime := &runtime{input: clientInput, inbound: map[string]struct{}{requestIDKey(t, rpcID): {}}}
	requestID := testID(22)
	request := provider.InteractionRequest{
		ID: requestID, Kind: provider.InteractionPermissionApproval, Title: "Approve permissions",
		Options: []provider.InteractionOption{{ID: "grantTurn", Label: "Allow for turn"}, {ID: "decline", Label: "Decline"}},
		Fields: []provider.InteractionField{{ID: "permissions", Label: "Permissions", Type: provider.InteractionMultiSelect, Options: []provider.InteractionOption{
			{ID: "permission0", Label: "Network"}, {ID: "permission1", Label: "Missing native mapping"},
		}}},
	}
	session := &Session{runtime: runtime, interactions: map[string]nativeInteraction{requestID: {
		rpcID: rpcID, method: "item/permissions/requestApproval", request: request,
		permissionChoices: map[string]nativePermissionChoice{"permission0": {kind: "network", value: json.RawMessage(`{"enabled":true}`)}},
	}}}

	err := session.Respond(context.Background(), provider.InteractionResponse{
		RequestID: requestID, Kind: request.Kind, OptionID: "grantTurn", Answers: map[string][]string{"permissions": {"permission1"}},
	})
	assertProviderError(t, err, provider.ErrorProtocolFailure)
	require.Contains(t, session.interactions, requestID)
	require.Contains(t, runtime.inbound, requestIDKey(t, rpcID))

	written := make(chan struct{}, 1)
	go func() {
		scanner := bufio.NewScanner(serverInput)
		if scanner.Scan() {
			written <- struct{}{}
		}
	}()
	require.NoError(t, session.Respond(context.Background(), provider.InteractionResponse{
		RequestID: requestID, Kind: request.Kind, OptionID: "grantTurn", Answers: map[string][]string{"permissions": {"permission0"}},
	}))
	<-written
	require.NoError(t, clientInput.Close())
}

func TestInteractionEncodingFailurePreservesRequestForRetry(t *testing.T) {
	serverInput, clientInput := io.Pipe()
	rpcID := json.RawMessage(`"native-number-request"`)
	runtime := &runtime{input: clientInput, inbound: map[string]struct{}{requestIDKey(t, rpcID): {}}}
	requestID := testID(23)
	request := provider.InteractionRequest{
		ID: requestID, Kind: provider.InteractionMCPElicitation, Title: "MCP server needs input",
		Options: []provider.InteractionOption{{ID: "accept", Label: "Accept"}, {ID: "decline", Label: "Decline"}},
		Fields:  []provider.InteractionField{{ID: "field0", Label: "Number", Type: provider.InteractionNumber, Required: true}},
	}
	pending := nativeInteraction{
		rpcID: rpcID, method: "mcpServer/elicitation/request", request: request,
		responseKey: map[string]string{"field0": "native-number"}, fieldTypes: map[string]provider.InteractionFieldType{"field0": provider.InteractionNumber},
	}
	session := &Session{runtime: runtime, interactions: map[string]nativeInteraction{requestID: pending}}

	for _, values := range [][]string{{"NaN"}, {"+Inf"}, {"1", "2"}} {
		err := session.Respond(context.Background(), provider.InteractionResponse{
			RequestID: requestID, Kind: request.Kind, OptionID: "accept", Answers: map[string][]string{"field0": values},
		})
		assertProviderError(t, err, provider.ErrorProtocolFailure)
		require.Contains(t, session.interactions, requestID)
		require.Contains(t, runtime.inbound, requestIDKey(t, rpcID))
	}

	written := make(chan map[string]json.RawMessage, 1)
	go func() {
		scanner := bufio.NewScanner(serverInput)
		if scanner.Scan() {
			var response map[string]json.RawMessage
			_ = json.Unmarshal(scanner.Bytes(), &response)
			written <- response
		}
	}()
	require.NoError(t, session.Respond(context.Background(), provider.InteractionResponse{
		RequestID: requestID, Kind: request.Kind, OptionID: "accept", Answers: map[string][]string{"field0": {"1.5"}},
	}))
	response := <-written
	require.JSONEq(t, `{"action":"accept","content":{"native-number":1.5}}`, string(response["result"]))
	require.NoError(t, clientInput.Close())
}

func TestUnencodableInteractionRPCResultPreservesRequestForRetry(t *testing.T) {
	serverInput, clientInput := io.Pipe()
	rpcID := json.RawMessage(`"native-permission-request"`)
	runtime := &runtime{input: clientInput, inbound: map[string]struct{}{requestIDKey(t, rpcID): {}}}
	requestID := testID(24)
	request := provider.InteractionRequest{
		ID: requestID, Kind: provider.InteractionPermissionApproval, Title: "Approve permissions",
		Options: []provider.InteractionOption{{ID: "grantTurn", Label: "Allow for turn"}},
		Fields:  []provider.InteractionField{{ID: "permissions", Label: "Permissions", Type: provider.InteractionMultiSelect, Options: []provider.InteractionOption{{ID: "permission0", Label: "Network"}}}},
	}
	pending := nativeInteraction{
		rpcID: rpcID, method: "item/permissions/requestApproval", request: request,
		permissionChoices: map[string]nativePermissionChoice{"permission0": {kind: "network", value: json.RawMessage(`{`)}},
	}
	session := &Session{runtime: runtime, interactions: map[string]nativeInteraction{requestID: pending}}
	response := provider.InteractionResponse{
		RequestID: requestID, Kind: request.Kind, OptionID: "grantTurn", Answers: map[string][]string{"permissions": {"permission0"}},
	}

	assertProviderError(t, session.Respond(context.Background(), response), provider.ErrorProtocolFailure)
	require.Contains(t, session.interactions, requestID)
	require.Contains(t, runtime.inbound, requestIDKey(t, rpcID))
	pending.permissionChoices["permission0"] = nativePermissionChoice{kind: "network", value: json.RawMessage(`{"enabled":true}`)}

	written := make(chan struct{}, 1)
	go func() {
		scanner := bufio.NewScanner(serverInput)
		if scanner.Scan() {
			written <- struct{}{}
		}
	}()
	require.NoError(t, session.Respond(context.Background(), response))
	<-written
	require.NoError(t, clientInput.Close())
}

func TestServerRequestResolvedConsumesNativeAndBrowserInteractionExactlyOnce(t *testing.T) {
	rpcID := json.RawMessage(`700`)
	runtime := &runtime{input: discardWriteCloser{}, inbound: map[string]struct{}{requestIDKey(t, rpcID): {}}}
	requestID := testID(3)
	request := provider.InteractionRequest{
		ID: requestID, TurnID: testID(4), Kind: provider.InteractionCommandApproval, Title: "Approve command",
		Options: approvalOptions(),
	}
	session := &Session{
		driver: &Driver{config: Config{IDs: &sequenceIDs{}}}, runtime: runtime, events: make(chan provider.Event, 2),
		interactions: map[string]nativeInteraction{requestID: {rpcID: rpcID, method: "item/commandExecution/requestApproval", request: request}},
		activities:   make(map[string]string), toolStates: make(map[string]provider.ToolActivity),
	}

	session.handleNotification("serverRequest/resolved", json.RawMessage(`{"threadId":"thread","requestId":700}`))
	resolved := awaitEvent(t, session.events)
	require.Equal(t, provider.EventInteractionResolved, resolved.Kind)
	require.Equal(t, requestID, resolved.Resolution.RequestID)
	require.Equal(t, request.Kind, resolved.Resolution.Kind)
	require.Empty(t, resolved.Resolution.OptionID)
	require.Empty(t, session.interactions)
	require.Empty(t, runtime.inbound)
	require.Error(t, session.Respond(context.Background(), provider.InteractionResponse{RequestID: requestID, Kind: request.Kind, OptionID: "accept"}))

	// A duplicate native resolution is stale and must not emit a second event.
	session.handleNotification("serverRequest/resolved", json.RawMessage(`{"threadId":"thread","requestId":700}`))
	select {
	case duplicate := <-session.events:
		t.Fatalf("unexpected duplicate resolution: %#v", duplicate)
	default:
	}
}

func TestServerRequestResolvedCorrelatesStringNativeID(t *testing.T) {
	rpcID := json.RawMessage(`"native-request"`)
	runtime := &runtime{input: discardWriteCloser{}, inbound: map[string]struct{}{requestIDKey(t, rpcID): {}}}
	requestID := testID(5)
	request := provider.InteractionRequest{ID: requestID, Kind: provider.InteractionCommandApproval, Title: "Approve command", Options: approvalOptions()}
	session := &Session{
		runtime: runtime, events: make(chan provider.Event, 1),
		interactions: map[string]nativeInteraction{requestID: {rpcID: rpcID, method: "item/commandExecution/requestApproval", request: request}},
	}
	session.handleServerRequestResolved(json.RawMessage(`{"requestId":"native-request"}`))
	event := awaitEvent(t, session.events)
	require.Equal(t, provider.EventInteractionResolved, event.Kind)
	require.Equal(t, requestID, event.Resolution.RequestID)
	require.Empty(t, session.interactions)
	require.Empty(t, runtime.inbound)
}

func TestConcurrentInboundOutcomesAtomicallyClaimPendingRequest(t *testing.T) {
	for _, outcome := range []string{"respond", "cancel", "shutdown"} {
		t.Run(outcome, func(t *testing.T) {
			for iteration := 0; iteration < 50; iteration++ {
				rpcID := json.RawMessage(strconv.Itoa(900 + iteration))
				runtime := &runtime{input: discardWriteCloser{}, inbound: map[string]struct{}{requestIDKey(t, rpcID): {}}, pending: make(map[int64]chan rpcResult), sessions: make(map[string]*Session), done: make(chan struct{})}
				driver := &Driver{config: Config{IDs: &sequenceIDs{}, IdleTimeout: time.Hour}, runtime: runtime}
				requestID := testID(uint64(600 + iteration))
				request := provider.InteractionRequest{ID: requestID, Kind: provider.InteractionCommandApproval, Title: "Approve command", Options: approvalOptions()}
				session := &Session{
					driver: driver, runtime: runtime, threadID: "native-thread", events: make(chan provider.Event, 2), view: newSessionChild(),
					interactions: map[string]nativeInteraction{requestID: {rpcID: rpcID, method: "item/commandExecution/requestApproval", request: request}},
					activities:   make(map[string]string),
				}
				start := make(chan struct{})
				done := make(chan struct{}, 2)
				go func() {
					<-start
					switch outcome {
					case "respond":
						_ = session.Respond(context.Background(), provider.InteractionResponse{RequestID: requestID, Kind: request.Kind, OptionID: "accept"})
					case "cancel":
						_ = session.CancelInteraction(context.Background(), requestID)
					case "shutdown":
						_ = session.Shutdown(context.Background())
					}
					done <- struct{}{}
				}()
				go func() {
					<-start
					session.handleServerRequestResolved(mustJSON(t, map[string]any{"requestId": 900 + iteration}))
					done <- struct{}{}
				}()
				close(start)
				<-done
				<-done
				session.mu.Lock()
				require.Empty(t, session.interactions)
				session.mu.Unlock()
				runtime.mu.Lock()
				require.Empty(t, runtime.inbound)
				runtime.mu.Unlock()
				driver.mu.Lock()
				if driver.idleTimer != nil {
					driver.idleTimer.Stop()
				}
				driver.mu.Unlock()
			}
		})
	}
}

func TestMCPFormTitledEnumsPreserveOpaqueKeysAndNativeValues(t *testing.T) {
	session := &Session{
		driver: &Driver{config: Config{IDs: &sequenceIDs{}}}, interactions: make(map[string]nativeInteraction), activities: make(map[string]string),
		active: &nativeTurn{request: provider.TurnRequest{TurnID: testID(9)}, nativeID: "turn"},
	}
	params := json.RawMessage(`{
		"threadId":"thread","turnId":"turn","serverName":"fixture","mode":"form","message":"Choose",
		"requestedSchema":{"type":"object","properties":{"native field":{"type":"string","title":"Target","oneOf":[{"const":"native value","title":"Friendly value"}]}},"required":["native field"]}
	}`)
	request, pending, err := session.normalizeInteraction(testID(1), "mcpServer/elicitation/request", params)
	require.NoError(t, err)
	require.Len(t, request.Fields, 1)
	require.Equal(t, provider.InteractionSelect, request.Fields[0].Type)
	require.Equal(t, "Friendly value", request.Fields[0].Options[0].Label)
	result, err := interactionRPCResult(pending, provider.InteractionResponse{
		RequestID: request.ID, Kind: request.Kind, OptionID: "accept",
		Answers: map[string][]string{"field0": {"option0"}},
	})
	require.NoError(t, err)
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	require.JSONEq(t, `{"action":"accept","content":{"native field":"native value"}}`, string(encoded))
	declined, err := interactionRPCResult(pending, provider.InteractionResponse{RequestID: request.ID, Kind: request.Kind, OptionID: "decline"})
	require.NoError(t, err)
	declinedJSON, err := json.Marshal(declined)
	require.NoError(t, err)
	require.JSONEq(t, `{"action":"decline","content":null}`, string(declinedJSON))
}

func TestPermissionApprovalReturnsOnlyTheSelectedStablePermissionSubset(t *testing.T) {
	session := &Session{
		driver: &Driver{config: Config{IDs: &sequenceIDs{}}}, interactions: make(map[string]nativeInteraction), activities: make(map[string]string),
		active: &nativeTurn{request: provider.TurnRequest{TurnID: testID(9)}, nativeID: "turn"},
	}
	params := json.RawMessage(`{
		"threadId":"thread","turnId":"turn","itemId":"item","startedAtMs":1,"cwd":"/workspace","reason":"Run integration checks",
		"permissions":{
			"network":{"enabled":true},
			"fileSystem":{"globScanMaxDepth":5,"entries":[
				{"access":"read","path":{"type":"path","path":"/fixtures/input"}},
				{"access":"write","path":{"type":"path","path":"/fixtures/output"}}
			]}
		}
	}`)
	request, pending, err := session.normalizeInteraction(testID(1), "item/permissions/requestApproval", params)
	require.NoError(t, err)
	require.Len(t, request.Fields, 1)
	require.Equal(t, provider.InteractionMultiSelect, request.Fields[0].Type)
	require.Len(t, request.Fields[0].Options, 3)

	result, err := interactionRPCResult(pending, provider.InteractionResponse{
		RequestID: request.ID, Kind: request.Kind, OptionID: "grantSession",
		Answers: map[string][]string{"permissions": {"permission0", "permission2"}},
	})
	require.NoError(t, err)
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"scope":"session",
		"permissions":{
			"network":{"enabled":true},
			"fileSystem":{"globScanMaxDepth":5,"entries":[{"access":"write","path":{"type":"path","path":"/fixtures/output"}}]}
		}
	}`, string(encoded))

	_, err = interactionRPCResult(pending, provider.InteractionResponse{RequestID: request.ID, Kind: request.Kind, OptionID: "grantTurn"})
	require.Error(t, err)
	declined, err := interactionRPCResult(pending, provider.InteractionResponse{RequestID: request.ID, Kind: request.Kind, OptionID: "decline"})
	require.NoError(t, err)
	declinedJSON, err := json.Marshal(declined)
	require.NoError(t, err)
	require.JSONEq(t, `{"permissions":{},"scope":"turn"}`, string(declinedJSON))
}

func TestRuntimeRejectsIncompleteInitializeResponse(t *testing.T) {
	launcher := &scriptedLauncher{serve: func(child *scriptedChild) {
		scanner := bufio.NewScanner(child.serverInput)
		for scanner.Scan() {
			var request map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(scanner.Bytes(), &request))
			var method string
			_ = json.Unmarshal(request["method"], &method)
			if method == "initialize" {
				child.send(t, map[string]any{"id": request["id"], "result": map[string]any{"userAgent": "fixture"}})
			}
		}
	}}
	driver := &Driver{config: Config{
		Executable: "/fixture/bin/codex", Environment: []string{"PATH=/fixture/bin"}, ProviderRoot: t.TempDir(),
		Launcher: launcher, IDs: &sequenceIDs{}, Clock: fixedClock{time.Unix(100, 0).UTC()}, IdleTimeout: time.Hour,
	}}
	_, err := startRuntime(context.Background(), driver)
	require.Error(t, err)
}

func TestShutdownCancelsEveryPendingNativeRequestExactlyOnce(t *testing.T) {
	serverInput, clientInput := io.Pipe()
	runtime := &runtime{input: clientInput, inbound: make(map[string]struct{})}
	driver := &Driver{config: Config{IDs: &sequenceIDs{}}}
	session := &Session{
		driver: driver, runtime: runtime, events: make(chan provider.Event, 1), view: newSessionChild(),
		interactions: make(map[string]nativeInteraction), activities: make(map[string]string),
	}
	for index, method := range []string{"item/commandExecution/requestApproval", "item/tool/requestUserInput"} {
		requestID := testID(uint64(index + 1))
		rpcID := json.RawMessage(strconv.Itoa(index + 10))
		runtime.inbound[requestIDKey(t, rpcID)] = struct{}{}
		session.interactions[requestID] = nativeInteraction{rpcID: rpcID, method: method}
	}

	var output bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&output, serverInput)
		close(done)
	}()
	require.NoError(t, session.Shutdown(context.Background()))
	require.NoError(t, clientInput.Close())
	<-done
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	require.Len(t, lines, 2)
	require.Contains(t, output.String(), `"decision":"cancel"`)
	require.Contains(t, output.String(), `"answers":{}`)
}

func TestShutdownInterruptsActiveTurnAndStopsRuntimeWhenUnconfirmed(t *testing.T) {
	t.Run("confirmed interrupt preserves shared runtime", func(t *testing.T) {
		runtime, requests, child := pipeRuntime(t)
		driver := &Driver{config: Config{IDs: &sequenceIDs{}, IdleTimeout: time.Hour}, runtime: runtime}
		session := &Session{
			driver: driver, runtime: runtime, threadID: "native-thread", events: make(chan provider.Event, 1), view: newSessionChild(),
			active:       &nativeTurn{request: provider.TurnRequest{TurnID: testID(30)}, nativeID: "native-turn"},
			interactions: make(map[string]nativeInteraction), activities: make(map[string]string),
		}
		runtime.sessions[session.threadID] = session
		go func() {
			request := <-requests
			runtime.handleResponse(rpcEnvelope{ID: request["id"], Result: json.RawMessage(`{}`)})
		}()
		require.NoError(t, session.Shutdown(context.Background()))
		select {
		case <-runtime.done:
			t.Fatal("confirmed interrupt stopped shared runtime")
		default:
		}
		driver.mu.Lock()
		if driver.idleTimer != nil {
			driver.idleTimer.Stop()
		}
		driver.mu.Unlock()
		child.stop()
	})

	t.Run("unconfirmed interrupt stops runtime", func(t *testing.T) {
		runtime, requests, child := pipeRuntime(t)
		driver := &Driver{config: Config{IDs: &sequenceIDs{}, IdleTimeout: time.Hour}, runtime: runtime}
		session := &Session{
			driver: driver, runtime: runtime, threadID: "native-thread", events: make(chan provider.Event, 1), view: newSessionChild(),
			active:       &nativeTurn{request: provider.TurnRequest{TurnID: testID(31)}, nativeID: "native-turn"},
			interactions: make(map[string]nativeInteraction), activities: make(map[string]string),
		}
		runtime.sessions[session.threadID] = session
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-requests; cancel() }()
		require.NoError(t, session.Shutdown(ctx))
		select {
		case <-child.done:
		case <-time.After(time.Second):
			t.Fatal("unconfirmed interrupt did not stop runtime")
		}
		driver.mu.Lock()
		if driver.idleTimer != nil {
			driver.idleTimer.Stop()
		}
		driver.mu.Unlock()
	})
}

func TestConcurrentSubmitAndShutdownSynchronizeNativeTurnIdentity(t *testing.T) {
	for iteration := 0; iteration < 100; iteration++ {
		input := newSignalingWriteCloser()
		runtime := &runtime{input: input, pending: make(map[int64]chan rpcResult), barriers: make(map[int64]*responseBarrier), inbound: make(map[string]struct{}), sessions: make(map[string]*Session), done: make(chan struct{})}
		driver := &Driver{config: Config{Clock: fixedClock{time.Unix(100, 0).UTC()}, IDs: &sequenceIDs{}, IdleTimeout: time.Hour}, runtime: runtime}
		session := &Session{
			driver: driver, runtime: runtime, threadID: "native-thread", events: make(chan provider.Event, 4), view: newSessionChild(),
			activities: make(map[string]string), toolStates: make(map[string]provider.ToolActivity), interactions: make(map[string]nativeInteraction),
		}
		runtime.sessions[session.threadID] = session
		request := provider.TurnRequest{TurnID: testID(uint64(800 + iteration*2)), MessageID: testID(uint64(801 + iteration*2)), Message: "race"}
		submitDone := make(chan error, 1)
		go func() { _, err := session.Submit(context.Background(), request); submitDone <- err }()
		<-input.wrote

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := make(chan struct{})
		shutdownDone := make(chan error, 1)
		responseDone := make(chan struct{})
		go func() {
			<-start
			runtime.handleResponse(rpcEnvelope{ID: json.RawMessage(`1`), Result: json.RawMessage(`{"turn":{"id":"native-turn"}}`)})
			close(responseDone)
		}()
		go func() {
			<-start
			shutdownDone <- session.Shutdown(ctx)
		}()
		close(start)
		require.NoError(t, <-shutdownDone)
		<-responseDone
		_ = <-submitDone
		driver.mu.Lock()
		driver.stopIdleLocked()
		driver.mu.Unlock()
	}
}

func TestMalformedTurnCompletionFailsClosed(t *testing.T) {
	turnID := testID(40)
	session := &Session{
		events: make(chan provider.Event, 1), active: &nativeTurn{request: provider.TurnRequest{TurnID: turnID}, nativeID: "native-turn"},
		activities: make(map[string]string), toolStates: make(map[string]provider.ToolActivity),
	}
	session.handleNotification("turn/completed", json.RawMessage(`{"threadId":"native-thread","turn":{"status":"completed"}}`))
	event := awaitEvent(t, session.events)
	require.Equal(t, provider.EventTerminalFailure, event.Kind)
	require.Equal(t, provider.ErrorMalformedStream, event.Failure.Code())
	session.mu.Lock()
	require.Nil(t, session.active)
	session.mu.Unlock()
}

func TestRuntimeStopPreservesProviderCauseForPendingCallsAndSessions(t *testing.T) {
	runtime := &runtime{input: discardWriteCloser{}, pending: make(map[int64]chan rpcResult), inbound: make(map[string]struct{}), sessions: make(map[string]*Session), done: make(chan struct{})}
	session := &Session{events: make(chan provider.Event, 1), view: newSessionChild(), active: &nativeTurn{request: provider.TurnRequest{TurnID: testID(41)}}}
	runtime.sessions["thread"] = session
	runtime.stop(provider.NewProviderError(provider.ErrorMalformedStream))
	event := awaitEvent(t, session.events)
	require.Equal(t, provider.ErrorMalformedStream, event.Failure.Code())
}

func TestContextCancellationStopsRuntimeToUnblockWedgedWrite(t *testing.T) {
	input := newBlockingWriteCloser()
	child := &managedChildFixture{input: input, done: make(chan struct{})}
	runtime := &runtime{child: child, input: input, pending: make(map[int64]chan rpcResult), inbound: make(map[string]struct{}), sessions: make(map[string]*Session), done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := runtime.call(ctx, "thread/read", map[string]any{}); result <- err }()
	<-input.entered
	cancel()
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("cancellation did not unblock runtime write")
	}
	require.True(t, child.stopped())
}

func TestQueuedWriterCanCancelWithoutWaitingForWedgedWriter(t *testing.T) {
	input := newBlockingWriteCloser()
	runtime := &runtime{input: input, pending: make(map[int64]chan rpcResult), inbound: make(map[string]struct{}), sessions: make(map[string]*Session), done: make(chan struct{})}
	first := make(chan error, 1)
	go func() { first <- runtime.notify(context.Background(), "fixture/first", map[string]any{}) }()
	<-input.entered

	ctx, cancel := context.WithCancel(context.Background())
	second := make(chan error, 1)
	go func() {
		_, err := runtime.call(ctx, "fixture/second", map[string]any{})
		second <- err
	}()
	require.Eventually(t, func() bool {
		runtime.mu.Lock()
		defer runtime.mu.Unlock()
		return len(runtime.pending) == 1
	}, time.Second, time.Millisecond)
	cancel()
	select {
	case err := <-second:
		require.ErrorIs(t, err, context.Canceled)
		runtime.mu.Lock()
		require.Empty(t, runtime.pending)
		runtime.mu.Unlock()
	case <-time.After(100 * time.Millisecond):
		_ = input.Close()
		<-first
		<-second
		t.Fatal("queued writer remained blocked behind wedged writer")
	}
	require.NoError(t, input.Close())
	require.Error(t, <-first)
}

func TestIncrementalToolOutputUsesExistingOpaqueActivityAndStaysBounded(t *testing.T) {
	session := &Session{
		driver: &Driver{config: Config{IDs: &sequenceIDs{}, Clock: fixedClock{time.Unix(100, 0).UTC()}}},
		events: make(chan provider.Event, 8), activities: make(map[string]string), interactions: make(map[string]nativeInteraction),
		active: &nativeTurn{request: provider.TurnRequest{TurnID: testID(20)}, nativeID: "native-turn"},
	}
	session.handleNotification("item/started", mustJSON(t, map[string]any{
		"threadId": "native-thread", "turnId": "native-turn", "startedAtMs": 1,
		"item": map[string]any{"id": "native-item", "type": "commandExecution", "command": "go test", "commandActions": []any{}, "cwd": "/workspace", "status": "inProgress"},
	}))
	started := awaitEvent(t, session.events)
	require.Equal(t, provider.EventToolActivity, started.Kind)

	session.handleNotification("item/commandExecution/outputDelta", mustJSON(t, map[string]any{
		"threadId": "native-thread", "turnId": "native-turn", "itemId": "native-item", "delta": strings.Repeat("x", provider.MaxInteractionTextBytes*2),
	}))
	updated := awaitEvent(t, session.events)
	require.Equal(t, started.Tool.ID, updated.Tool.ID)
	require.LessOrEqual(t, len(updated.Tool.Detail), provider.MaxInteractionTextBytes)

	session.handleNotification("item/commandExecution/terminalInteraction", mustJSON(t, map[string]any{
		"threadId": "native-thread", "turnId": "native-turn", "itemId": "native-item", "processId": "process", "stdin": "yes\n",
	}))
	terminal := awaitEvent(t, session.events)
	require.Equal(t, started.Tool.ID, terminal.Tool.ID)
	require.Contains(t, terminal.Tool.Detail, "yes")

	session.handleNotification("turn/completed", mustJSON(t, map[string]any{
		"threadId": "native-thread", "turn": map[string]any{"id": "native-turn", "status": "completed"},
	}))
	require.Equal(t, provider.EventCompletion, awaitEvent(t, session.events).Kind)
	session.mu.Lock()
	require.Empty(t, session.activities)
	require.Empty(t, session.toolStates)
	session.mu.Unlock()
}

func TestHistoryProjectionBoundsNativeTextAndAggregateSize(t *testing.T) {
	turns := make([]any, 80)
	for index := range turns {
		turnID := testID(uint64(index + 100))
		envelope, err := contentturn.Build(provider.TurnRequest{
			TurnID: turnID, MessageID: testID(uint64(index + 200)), Message: "question",
		}, contentturn.PolicyConfigured)
		require.NoError(t, err)
		turns[index] = map[string]any{
			"id": "native", "status": "completed", "startedAt": 10,
			"items": []any{
				map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": string(envelope)}}},
				map[string]any{"type": "agentMessage", "text": strings.Repeat("a", provider.MaxHistoryItemBytes*2)},
			},
		}
	}
	items, err := projectHistory(mustJSON(t, map[string]any{"thread": map[string]any{"id": "native-thread", "turns": turns}}), "native-thread")
	require.NoError(t, err)
	page := provider.HistoryPage{Items: items}
	require.NoError(t, page.Validate())
}

func TestHistoryReturnsNewestFirstAndPagesTowardOlderMessages(t *testing.T) {
	turns := make([]any, 3)
	for index := range turns {
		turnID := testID(uint64(index + 300))
		messageID := testID(uint64(index + 400))
		envelope, err := contentturn.Build(provider.TurnRequest{
			TurnID: turnID, MessageID: messageID, Message: "question " + strconv.Itoa(index),
		}, contentturn.PolicyConfigured)
		require.NoError(t, err)
		turns[index] = map[string]any{
			"id": "native-" + strconv.Itoa(index), "status": "completed", "startedAt": index + 1,
			"items": []any{
				map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": string(envelope)}}},
				map[string]any{"type": "agentMessage", "text": "answer " + strconv.Itoa(index)},
			},
		}
	}
	items, err := projectHistory(mustJSON(t, map[string]any{"thread": map[string]any{"id": "native-thread", "turns": turns}}), "native-thread")
	require.NoError(t, err)
	page, err := historyPage(items, provider.HistoryRequest{Limit: 2})
	require.NoError(t, err)
	require.Equal(t, []string{"answer 2", "question 2"}, []string{page.Items[0].Text, page.Items[1].Text})
	require.Equal(t, testID(402), page.NextCursor)
	older, err := historyPage(items, provider.HistoryRequest{Limit: 2, BeforeMessageID: page.NextCursor})
	require.NoError(t, err)
	require.Equal(t, []string{"answer 1", "question 1"}, []string{older.Items[0].Text, older.Items[1].Text})
	require.Equal(t, testID(401), older.NextCursor)
}

func TestHistoryAndReconciliationRejectMissingOrWrongThread(t *testing.T) {
	for name, raw := range map[string]json.RawMessage{
		"missing thread": json.RawMessage(`{}`),
		"null thread":    json.RawMessage(`{"thread":null}`),
		"wrong thread":   json.RawMessage(`{"thread":{"id":"other","turns":[]}}`),
		"missing id":     json.RawMessage(`{"thread":{"turns":[]}}`),
	} {
		t.Run(name, func(t *testing.T) {
			_, historyErr := projectHistory(raw, "native-thread")
			require.Error(t, historyErr)
			state, reconcileErr := reconcileHistory(raw, "native-thread", testID(500))
			require.Equal(t, provider.TurnUnknown, state)
			require.Error(t, reconcileErr)
		})
	}
}

func TestReconciliationDoesNotTreatMalformedTurnsAsNotAccepted(t *testing.T) {
	raw := json.RawMessage(`{"thread":{"id":"native-thread","turns":[{"id":"native-turn","status":"completed"}]}}`)
	state, err := reconcileHistory(raw, "native-thread", testID(501))
	require.Equal(t, provider.TurnUnknown, state)
	require.Error(t, err)
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return encoded
}

func awaitEvent(t *testing.T, events <-chan provider.Event) provider.Event {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Codex event")
		return provider.Event{}
	}
}

func assertProviderError(t *testing.T, err error, code provider.ProviderErrorCode) {
	t.Helper()
	var typed provider.ProviderError
	require.ErrorAs(t, err, &typed)
	require.Equal(t, code, typed.Code())
}

func readyLauncher(t *testing.T, handle func(*scriptedChild, map[string]json.RawMessage, string)) *scriptedLauncher {
	t.Helper()
	return &scriptedLauncher{serve: func(child *scriptedChild) {
		scanner := bufio.NewScanner(child.serverInput)
		for scanner.Scan() {
			var request map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(scanner.Bytes(), &request))
			var method string
			_ = json.Unmarshal(request["method"], &method)
			switch method {
			case "initialize":
				child.send(t, map[string]any{"id": request["id"], "result": map[string]any{
					"codexHome": "/fixture/home", "platformFamily": "unix", "platformOs": "linux", "userAgent": "fixture",
				}})
			case "initialized":
			case "account/read":
				child.send(t, map[string]any{"id": request["id"], "result": map[string]any{"account": map[string]any{"type": "apiKey"}, "requiresOpenaiAuth": true}})
			default:
				handle(child, request, method)
			}
		}
	}}
}

func pipeRuntime(t *testing.T) (*runtime, <-chan map[string]json.RawMessage, *scriptedChild) {
	t.Helper()
	clientInput, serverInput := io.Pipe()
	serverOutput, clientOutput := io.Pipe()
	child := &scriptedChild{clientInput: serverInput, clientOutput: serverOutput, serverInput: clientInput, serverOutput: clientOutput, done: make(chan struct{})}
	runtime := &runtime{child: child, input: child.Input(), pending: make(map[int64]chan rpcResult), inbound: make(map[string]struct{}), sessions: make(map[string]*Session), done: make(chan struct{})}
	requests := make(chan map[string]json.RawMessage, 1)
	go func() {
		scanner := bufio.NewScanner(child.serverInput)
		for scanner.Scan() {
			var request map[string]json.RawMessage
			if json.Unmarshal(scanner.Bytes(), &request) == nil {
				requests <- request
			}
		}
	}()
	return runtime, requests, child
}

func requestIDKey(t *testing.T, id json.RawMessage) string {
	t.Helper()
	key, err := rpcRequestIDKey(id)
	require.NoError(t, err)
	return key
}

func notification(method string, params any) map[string]any {
	return map[string]any{"method": method, "params": params}
}

func sortedKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	if len(keys) == 2 && keys[0] > keys[1] {
		keys[0], keys[1] = keys[1], keys[0]
	}
	return keys
}

type signalingWriteCloser struct {
	wrote chan struct{}
	once  sync.Once
}

func newSignalingWriteCloser() *signalingWriteCloser {
	return &signalingWriteCloser{wrote: make(chan struct{})}
}

func (writer *signalingWriteCloser) Write(value []byte) (int, error) {
	writer.once.Do(func() { close(writer.wrote) })
	return len(value), nil
}

func (*signalingWriteCloser) Close() error { return nil }

type fixedClock struct{ now time.Time }

func (clock fixedClock) Now() time.Time { return clock.now }

type blockingClock struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
	now     time.Time
}

func (clock *blockingClock) Now() time.Time {
	clock.once.Do(func() { close(clock.entered) })
	<-clock.release
	return clock.now
}

type sequenceIDs struct {
	mu   sync.Mutex
	next uint64
}

func (ids *sequenceIDs) NewID() (string, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	ids.next++
	return testID(ids.next), nil
}

func testID(value uint64) string {
	raw := make([]byte, 24)
	binary.BigEndian.PutUint64(raw[16:], value)
	return base64.RawURLEncoding.EncodeToString(raw)
}

type scriptedLauncher struct {
	mu       sync.Mutex
	requests []provider.LaunchRequest
	children []*scriptedChild
	serve    func(*scriptedChild)
}

func (launcher *scriptedLauncher) Launch(_ context.Context, request provider.LaunchRequest) (provider.ManagedChild, error) {
	launcher.mu.Lock()
	launcher.requests = append(launcher.requests, request)
	launcher.mu.Unlock()
	clientInput, serverInput := io.Pipe()
	serverOutput, clientOutput := io.Pipe()
	child := &scriptedChild{clientInput: serverInput, clientOutput: serverOutput, serverInput: clientInput, serverOutput: clientOutput, done: make(chan struct{})}
	launcher.mu.Lock()
	launcher.children = append(launcher.children, child)
	launcher.mu.Unlock()
	go launcher.serve(child)
	return child, nil
}

type failingLauncher struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (launcher *failingLauncher) Launch(context.Context, provider.LaunchRequest) (provider.ManagedChild, error) {
	launcher.mu.Lock()
	launcher.calls++
	launcher.mu.Unlock()
	launcher.once.Do(func() { close(launcher.started) })
	<-launcher.release
	return nil, errors.New("fixture startup failure")
}

type blockingWriteCloser struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingWriteCloser() *blockingWriteCloser {
	return &blockingWriteCloser{entered: make(chan struct{}), release: make(chan struct{})}
}

func (writer *blockingWriteCloser) Write([]byte) (int, error) {
	writer.once.Do(func() { close(writer.entered) })
	<-writer.release
	return 0, io.ErrClosedPipe
}

func (writer *blockingWriteCloser) Close() error {
	select {
	case <-writer.release:
	default:
		close(writer.release)
	}
	return nil
}

type managedChildFixture struct {
	input *blockingWriteCloser
	done  chan struct{}
	once  sync.Once
}

func (child *managedChildFixture) Input() io.WriteCloser { return child.input }
func (*managedChildFixture) Output() io.Reader           { return bytes.NewReader(nil) }
func (*managedChildFixture) Errors() io.Reader           { return nil }
func (child *managedChildFixture) Wait() error           { <-child.done; return nil }
func (child *managedChildFixture) Terminate() error      { child.stop(); return nil }
func (child *managedChildFixture) Kill() error           { child.stop(); return nil }
func (child *managedChildFixture) stop() {
	child.once.Do(func() {
		_ = child.input.Close()
		close(child.done)
	})
}
func (child *managedChildFixture) stopped() bool {
	select {
	case <-child.done:
		return true
	default:
		return false
	}
}

type scriptedChild struct {
	clientInput  *io.PipeWriter
	clientOutput *io.PipeReader
	serverInput  *io.PipeReader
	serverOutput *io.PipeWriter
	done         chan struct{}
	once         sync.Once
}

func (child *scriptedChild) send(t *testing.T, value any) {
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	_, err = child.serverOutput.Write(append(encoded, '\n'))
	require.NoError(t, err)
}

func (child *scriptedChild) Input() io.WriteCloser { return child.clientInput }
func (child *scriptedChild) Output() io.Reader     { return child.clientOutput }
func (child *scriptedChild) Errors() io.Reader     { return nil }
func (child *scriptedChild) Wait() error           { <-child.done; return nil }
func (child *scriptedChild) Terminate() error      { child.stop(); return nil }
func (child *scriptedChild) Kill() error           { child.stop(); return nil }
func (child *scriptedChild) stop() {
	child.once.Do(func() {
		_ = child.clientInput.Close()
		_ = child.clientOutput.Close()
		_ = child.serverInput.Close()
		_ = child.serverOutput.Close()
		close(child.done)
	})
}

var _ common.IDGenerator = (*sequenceIDs)(nil)
var _ provider.Launcher = (*scriptedLauncher)(nil)
var _ provider.Launcher = (*failingLauncher)(nil)
var _ provider.ManagedChild = (*scriptedChild)(nil)
var _ provider.ManagedChild = (*managedChildFixture)(nil)
