//go:build unix

package pi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/provider"
	"github.com/stretchr/testify/require"
)

func TestSessionPreflightUsesConservativeNativeUsage(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		session, child := newBehaviorSession(t)
		request := provider.PreflightRequest{Turn: behaviorTurn(1, "hello")}
		answer := make(chan struct {
			result provider.PreflightResult
			err    error
		}, 1)
		go func() {
			result, err := session.Preflight(context.Background(), request)
			answer <- struct {
				result provider.PreflightResult
				err    error
			}{result, err}
		}()
		respondPreflightState(t, child, 0, 32768, 1024)
		stats := child.readCommand(t)
		require.Equal(t, "get_session_stats", stats["type"])
		child.writeRecord(t, responseRecord(stats, map[string]any{"contextUsage": map[string]any{"tokens": 0, "contextWindow": 32768}}))
		resolved := <-answer
		require.NoError(t, resolved.err)
		envelope, err := BuildEnvelope(request.Turn)
		require.NoError(t, err)
		require.Equal(t, len(envelope), resolved.result.EstimatedInputTokens)
		require.Equal(t, 16384, resolved.result.SafetyMarginTokens)
		require.Equal(t, 16384, resolved.result.EffectiveCapacityTokens)
	})

	t.Run("resumed missing usage", func(t *testing.T) {
		session, child := newBehaviorSession(t)
		answer := make(chan error, 1)
		go func() {
			_, err := session.Preflight(context.Background(), provider.PreflightRequest{Turn: behaviorTurn(2, "hello")})
			answer <- err
		}()
		respondPreflightState(t, child, 2, 32768, 1024)
		stats := child.readCommand(t)
		child.writeRecord(t, responseRecord(stats, map[string]any{"contextUsage": nil}))
		assertProviderCode(t, <-answer, provider.ErrorProtocolFailure)
	})

	t.Run("model capacity drift", func(t *testing.T) {
		session, child := newBehaviorSession(t)
		answer := make(chan error, 1)
		go func() {
			_, err := session.Preflight(context.Background(), provider.PreflightRequest{Turn: behaviorTurn(3, "hello")})
			answer <- err
		}()
		respondPreflightState(t, child, 0, 65536, 1024)
		assertProviderCode(t, <-answer, provider.ErrorProtocolIncompatible)
	})
}

func TestSessionSubmitNormalizesRealPiEventSequence(t *testing.T) {
	session, child := newBehaviorSession(t)
	request := behaviorTurn(10, "reader question")
	answer := make(chan struct {
		accepted provider.AcceptedTurn
		err      error
	}, 1)
	go func() {
		accepted, err := session.Submit(context.Background(), request)
		answer <- struct {
			accepted provider.AcceptedTurn
			err      error
		}{accepted, err}
	}()
	command := child.readCommand(t)
	require.Equal(t, "prompt", command["type"])
	envelope, ok := command["message"].(string)
	require.True(t, ok)
	parsed, err := ParseEnvelope([]byte(envelope))
	require.NoError(t, err)
	require.Equal(t, request.TurnID, parsed.TurnID)

	child.writeRecord(t, map[string]any{"type": "agent_start"})
	child.writeRecord(t, nativeMessageRecord("message_start", "user", envelope, "stop"))
	child.writeRecord(t, map[string]any{"type": "turn_start"})
	child.writeRecord(t, nativeMessageRecord("message_start", "assistant", []any{}, ""))
	for _, update := range []map[string]any{
		{"type": "start"}, {"type": "text_start"}, {"type": "text_delta", "delta": "visible "},
		{"type": "thinking_delta", "delta": "hidden reasoning"}, {"type": "text_delta", "delta": "answer"},
		{"type": "text_end"}, {"type": "done", "reason": "stop"},
	} {
		child.writeRecord(t, map[string]any{"type": "message_update", "assistantMessageEvent": update})
	}
	// Acceptance may follow native events; the adapter must preserve the exact boundary.
	child.writeRecord(t, responseRecord(command, nil))
	resolved := <-answer
	require.NoError(t, resolved.err)
	require.Equal(t, request.TurnID, resolved.accepted.TurnID)
	child.writeRecord(t, nativeMessageRecord("message_end", "assistant", []any{
		map[string]any{"type": "thinking", "thinking": "must not escape"},
		map[string]any{"type": "text", "text": "visible answer"},
	}, "stop"))
	child.writeRecord(t, map[string]any{"type": "turn_end", "message": map[string]any{}})
	child.writeRecord(t, map[string]any{"type": "agent_end", "messages": []any{}})
	child.writeRecord(t, map[string]any{"type": "agent_settled"})

	events := receiveProviderEvents(t, session.Events(), 4)
	require.Equal(t, []provider.EventKind{provider.EventUserMessage, provider.EventAssistantDelta, provider.EventAssistantDelta, provider.EventAssistantMessage}, []provider.EventKind{events[0].Kind, events[1].Kind, events[2].Kind, events[3].Kind})
	require.Equal(t, request.Message, events[0].Text)
	require.Equal(t, "visible ", events[1].Text)
	require.Equal(t, "answer", events[2].Text)
	require.Equal(t, "visible answer", events[3].Text)
	for _, event := range events {
		require.NotContains(t, event.Text, "hidden")
		require.NotContains(t, event.Text, "agent-whiteboard-turn")
	}
	completion := receiveProviderEvents(t, session.Events(), 1)[0]
	require.Equal(t, provider.EventCompletion, completion.Kind)
}

func TestSessionSubmitNegativeAndAmbiguousAcceptance(t *testing.T) {
	for _, nativeEvent := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejected", true: "native event makes ambiguous"}[nativeEvent], func(t *testing.T) {
			session, child := newBehaviorSession(t)
			answer := make(chan error, 1)
			go func() { _, err := session.Submit(context.Background(), behaviorTurn(20, "question")); answer <- err }()
			command := child.readCommand(t)
			if nativeEvent {
				child.writeRecord(t, map[string]any{"type": "agent_start"})
			}
			child.writeRecord(t, map[string]any{"id": command["id"], "type": "response", "command": "prompt", "success": false, "error": "native secret"})
			err := <-answer
			if nativeEvent {
				assertProviderCode(t, err, provider.ErrorAcceptanceUnknown)
			} else {
				assertProviderCode(t, err, provider.ErrorProtocolFailure)
			}
			require.NotContains(t, err.Error(), "secret")
		})
	}
}

func TestSessionToleratesConfiguredToolActivityAndInterruptWaitsForSettlement(t *testing.T) {
	t.Run("tool activity", func(t *testing.T) {
		session, child := newBehaviorSession(t)
		turn := installBehaviorTurn(session, 30)
		child.writeRecord(t, map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "toolcall_start", "toolCall": map[string]any{"arguments": "secret"}}})
		child.writeRecord(t, map[string]any{"type": "tool_execution_start", "args": map[string]any{"token": "secret"}})
		child.writeRecord(t, map[string]any{"type": "agent_settled"})
		events := receiveProviderEvents(t, session.Events(), 3)
		require.Equal(t, []provider.EventKind{provider.EventActivity, provider.EventActivity, provider.EventCompletion}, []provider.EventKind{events[0].Kind, events[1].Kind, events[2].Kind})
		for _, event := range events[:2] {
			require.Equal(t, provider.ActivityStatus, event.Activity)
			require.NotContains(t, event.Text, "secret")
		}
		<-turn.settled
	})

	t.Run("interrupt", func(t *testing.T) {
		session, child := newBehaviorSession(t)
		turn := installBehaviorTurn(session, 31)
		accepted := provider.AcceptedTurn{TurnID: turn.request.TurnID, AcceptedAt: turn.acceptedAt}
		answer := make(chan error, 1)
		go func() { answer <- session.Interrupt(context.Background(), accepted) }()
		abort := child.readCommand(t)
		child.writeRecord(t, responseRecord(abort, nil))
		select {
		case <-answer:
			t.Fatal("interrupt returned before agent_settled")
		case <-time.After(20 * time.Millisecond):
		}
		child.writeRecord(t, map[string]any{"type": "agent_settled"})
		require.NoError(t, <-answer)
		event := receiveProviderEvents(t, session.Events(), 1)[0]
		require.Equal(t, provider.EventInterruption, event.Kind)
		require.Equal(t, provider.InterruptionRequested, event.Interruption)
	})
}

func TestSessionInterruptPreCanceledAttemptCanRetry(t *testing.T) {
	session, child := newBehaviorSession(t)
	turn := installBehaviorTurn(session, 32)
	accepted := provider.AcceptedTurn{TurnID: turn.request.TurnID, AcceptedAt: turn.acceptedAt}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, session.Interrupt(ctx, accepted), context.Canceled)
	session.mu.Lock()
	require.False(t, turn.interruptRequested)
	require.False(t, turn.abortSent)
	session.mu.Unlock()
	answer := make(chan error, 1)
	go func() { answer <- session.Interrupt(context.Background(), accepted) }()
	abort := child.readCommand(t)
	require.Equal(t, "abort", abort["type"])
	child.writeRecord(t, responseRecord(abort, nil))
	child.writeRecord(t, map[string]any{"type": "agent_settled"})
	require.NoError(t, <-answer)
}

func TestSessionDoesNotEmitIdleNativeActivity(t *testing.T) {
	session, child := newBehaviorSession(t)
	child.writeRecord(t, map[string]any{"type": "auto_retry_start", "errorMessage": "secret"})
	select {
	case event := <-session.Events():
		t.Fatalf("unexpected idle provider event: %#v", event)
	case <-time.After(30 * time.Millisecond):
	}
	turn := installBehaviorTurn(session, 34)
	session.mu.Lock()
	turn.userEmitted = false
	session.mu.Unlock()
	child.writeRecord(t, map[string]any{"type": "auto_retry_start", "errorMessage": "secret-before-envelope"})
	select {
	case event := <-session.Events():
		t.Fatalf("unexpected pre-envelope provider event: %#v", event)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestSessionRejectsAssistantOutputBeforeValidatedEnvelope(t *testing.T) {
	session, child := newBehaviorSession(t)
	turn := installBehaviorTurn(session, 33)
	session.mu.Lock()
	turn.userEmitted = false
	session.mu.Unlock()
	child.writeRecord(t, map[string]any{"type": "message_update", "assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "untrusted"}})
	event := receiveProviderEvents(t, session.Events(), 1)[0]
	require.Equal(t, provider.EventTerminalFailure, event.Kind)
	require.Equal(t, provider.ErrorMalformedStream, event.Failure.Code())
	require.NotContains(t, event.Text, "untrusted")
}

func TestSessionHistoryPagesNewestFirstOnActiveBranch(t *testing.T) {
	session, child := newBehaviorSession(t)
	entries, ids := behaviorHistoryEntries(t)
	pageAnswer := make(chan struct {
		page provider.HistoryPage
		err  error
	}, 1)
	go func() {
		page, err := session.History(context.Background(), provider.HistoryRequest{Limit: 2})
		pageAnswer <- struct {
			page provider.HistoryPage
			err  error
		}{page, err}
	}()
	respondEntries(t, child, entries, ids[3])
	first := <-pageAnswer
	require.NoError(t, first.err)
	require.Len(t, first.page.Items, 2)
	require.Equal(t, behaviorID(2), first.page.Items[0].TurnID)
	require.Equal(t, provider.HistoryAssistant, first.page.Items[0].Role)
	require.Equal(t, provider.HistoryUser, first.page.Items[1].Role)
	require.Equal(t, first.page.Items[1].MessageID, first.page.NextCursor)

	go func() {
		page, err := session.History(context.Background(), provider.HistoryRequest{Limit: 2, BeforeMessageID: first.page.NextCursor})
		pageAnswer <- struct {
			page provider.HistoryPage
			err  error
		}{page, err}
	}()
	respondEntries(t, child, entries, ids[3])
	second := <-pageAnswer
	require.NoError(t, second.err)
	require.Len(t, second.page.Items, 2)
	require.Equal(t, behaviorID(1), second.page.Items[0].TurnID)
	require.Empty(t, second.page.NextCursor)
}

func TestSessionReconcileUsesActiveLeafWithoutPromptReplay(t *testing.T) {
	session, child := newBehaviorSession(t)
	entries, ids := behaviorHistoryEntries(t)
	// Add an abandoned branch containing the same turn ID with malformed content;
	// it must not affect the active leaf walk.
	parent := ids[0]
	entries = append(entries, nativeEntry{ID: "abandoned", ParentID: &parent, Type: "message", Message: behaviorNativeMessage("user", "not an envelope", "stop", 6)})
	answer := make(chan struct {
		state provider.TurnState
		err   error
	}, 1)
	go func() {
		state, err := session.Reconcile(context.Background(), provider.TurnReference{TurnID: behaviorID(2)})
		answer <- struct {
			state provider.TurnState
			err   error
		}{state, err}
	}()
	respondEntries(t, child, entries, ids[3])
	resolved := <-answer
	require.NoError(t, resolved.err)
	require.Equal(t, provider.TurnCompleted, resolved.state)
}

func TestSessionShutdownIsContextBoundedAndRetryable(t *testing.T) {
	session, child := newBehaviorSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, session.Shutdown(ctx), context.DeadlineExceeded)
	child.closeOutput()
	require.NoError(t, session.Shutdown(context.Background()))
}

func newBehaviorSession(t *testing.T) (*Session, *rpcFakeChild) {
	t.Helper()
	child := newRPCFakeChild()
	client, err := newRPCClient(child)
	require.NoError(t, err)
	ref, err := provider.NewNativeSessionRef(behaviorID(90))
	require.NoError(t, err)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	driver := &Driver{config: Config{Clock: fixedClock{value: now}}, active: make(map[string]provider.Session), unfinalized: make(map[string]nativeAllocation)}
	state := startupState{SessionID: "session", SessionFile: "/tmp/session", Workspace: "/tmp/workspace", ModelProvider: "model-provider", ModelID: "model-id", Model: "model-provider/model-id", ContextWindow: 32768, MaxTokens: 1024}
	native := provider.NativeSession{Ref: ref, Provider: provider.NamePi, Model: state.Model, CreatedAt: now, UpdatedAt: now}
	session := newSession(driver, native, state, child, client)
	driver.active[ref.Value()] = session
	session.start()
	t.Cleanup(func() {
		child.closeOutput()
		select {
		case <-session.loopsDone:
		case <-time.After(time.Second):
			t.Errorf("session loops did not stop")
		}
	})
	return session, child
}

func respondPreflightState(t *testing.T, child *rpcFakeChild, messageCount, contextWindow, maxTokens int) {
	t.Helper()
	command := child.readCommand(t)
	require.Equal(t, "get_state", command["type"])
	child.writeRecord(t, responseRecord(command, map[string]any{
		"model":       map[string]any{"provider": "model-provider", "id": "model-id", "contextWindow": contextWindow, "maxTokens": maxTokens},
		"isStreaming": false, "isCompacting": false, "pendingMessageCount": 0, "messageCount": messageCount,
	}))
}

func responseRecord(command map[string]any, data any) map[string]any {
	result := map[string]any{"id": command["id"], "type": "response", "command": command["type"], "success": true}
	if data != nil {
		result["data"] = data
	}
	return result
}

func nativeMessageRecord(eventType, role string, content any, stopReason string) map[string]any {
	return map[string]any{"type": eventType, "message": map[string]any{"role": role, "content": content, "stopReason": stopReason, "timestamp": float64(1767323045000)}}
}

func receiveProviderEvents(t *testing.T, events <-chan provider.Event, count int) []provider.Event {
	t.Helper()
	result := make([]provider.Event, 0, count)
	for len(result) < count {
		select {
		case event, open := <-events:
			require.True(t, open)
			result = append(result, event)
		case <-time.After(time.Second):
			t.Fatalf("timed out after %d provider events", len(result))
		}
	}
	return result
}

func installBehaviorTurn(session *Session, seed byte) *activeTurn {
	request := behaviorTurn(seed, "reader")
	envelope, _ := BuildEnvelope(request)
	turn := &activeTurn{request: request, envelope: envelope, assistantID: assistantMessageID(request.TurnID), accepted: true, acceptedAt: session.driver.config.Clock.Now(), userEmitted: true, settled: make(chan struct{})}
	session.mu.Lock()
	session.active = turn
	session.mu.Unlock()
	return turn
}

func behaviorTurn(seed byte, message string) provider.TurnRequest {
	return provider.TurnRequest{TurnID: behaviorID(seed), MessageID: behaviorID(seed + 40), Message: message}
}
func behaviorID(seed byte) string {
	raw := make([]byte, 24)
	for index := range raw {
		raw[index] = seed
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func behaviorHistoryEntries(t *testing.T) ([]nativeEntry, []string) {
	t.Helper()
	ids := []string{"entry-1", "entry-2", "entry-3", "entry-4"}
	first := behaviorTurn(1, "first")
	second := behaviorTurn(2, "second")
	firstEnvelope, err := BuildEnvelope(first)
	require.NoError(t, err)
	secondEnvelope, err := BuildEnvelope(second)
	require.NoError(t, err)
	entries := []nativeEntry{
		{ID: ids[0], Type: "message", Message: behaviorNativeMessage("user", string(firstEnvelope), "", 1)},
		{ID: ids[1], ParentID: &ids[0], Type: "message", Message: behaviorNativeMessage("assistant", "answer one", "stop", 2)},
		{ID: ids[2], ParentID: &ids[1], Type: "message", Message: behaviorNativeMessage("user", string(secondEnvelope), "", 3)},
		{ID: ids[3], ParentID: &ids[2], Type: "message", Message: behaviorNativeMessage("assistant", "answer two", "stop", 4)},
	}
	return entries, ids
}

func behaviorNativeMessage(role, text, stopReason string, second int64) json.RawMessage {
	encoded, _ := json.Marshal(map[string]any{"role": role, "content": []any{map[string]any{"type": "text", "text": text}}, "stopReason": stopReason, "timestamp": time.Date(2026, 1, 2, 3, 4, int(second), 0, time.UTC).UnixMilli()})
	return encoded
}

func respondEntries(t *testing.T, child *rpcFakeChild, entries []nativeEntry, leaf string) {
	t.Helper()
	command := child.readCommand(t)
	require.Equal(t, "get_entries", command["type"])
	child.writeRecord(t, responseRecord(command, map[string]any{"entries": entries, "leafId": leaf}))
}

type recordingLauncher struct {
	child    provider.ManagedChild
	err      error
	requests chan provider.LaunchRequest
}

func (launcher *recordingLauncher) Launch(_ context.Context, request provider.LaunchRequest) (provider.ManagedChild, error) {
	launcher.requests <- request
	return launcher.child, launcher.err
}

func TestDriverRejectsTypedNilDependencies(t *testing.T) {
	root := canonicalTempDir(t)
	providerRoot := filepath.Join(root, "provider")
	executable := filepath.Join(root, "pi")
	require.NoError(t, os.Mkdir(providerRoot, 0o700))
	require.NoError(t, os.WriteFile(executable, []byte("binary"), 0o700))
	var launcher *recordingLauncher
	var ids *fixedIDs
	var clock *fixedClock
	_, err := NewDriver(Config{Executable: executable, Environment: []string{}, ProviderRoot: providerRoot, Launcher: launcher, IDs: ids, Clock: clock})
	require.Error(t, err)
}

func TestDriverRetainsChildReturnedWithLaunchError(t *testing.T) {
	root := canonicalTempDir(t)
	providerRoot := filepath.Join(root, "provider")
	workspace := filepath.Join(root, "workspace")
	executable := filepath.Join(root, "pi")
	require.NoError(t, os.Mkdir(providerRoot, 0o700))
	require.NoError(t, os.Mkdir(workspace, 0o700))
	require.NoError(t, os.WriteFile(executable, []byte("binary"), 0o700))
	child := newRPCFakeChild()
	launcher := &recordingLauncher{child: child, err: errors.New("post-start failure"), requests: make(chan provider.LaunchRequest, 1)}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	driver, err := NewDriver(Config{Executable: executable, Environment: []string{}, ProviderRoot: providerRoot, Launcher: launcher, IDs: fixedIDs{value: behaviorID(79)}, Clock: fixedClock{value: now}})
	require.NoError(t, err)
	result := make(chan struct {
		session provider.Session
		err     error
	}, 1)
	go func() {
		session, createErr := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NamePi, Access: provider.AccessConfigured, Workspace: workspace})
		result <- struct {
			session provider.Session
			err     error
		}{session, createErr}
	}()
	<-launcher.requests
	resolved := <-result
	require.Error(t, resolved.err)
	require.NotNil(t, resolved.session)
	require.NoError(t, resolved.session.NativeSession().Validate())
	deleteRequest := provider.DeleteRequest{Provider: provider.NamePi, NativeSession: resolved.session.NativeSession().Ref}
	assertProviderCode(t, driver.Delete(context.Background(), deleteRequest), provider.ErrorProtocolFailure)
	shutdown := make(chan error, 1)
	go func() { shutdown <- resolved.session.Shutdown(context.Background()) }()
	child.closeOutput()
	require.NoError(t, <-shutdown)
	require.NoError(t, driver.Delete(context.Background(), deleteRequest))
}

func TestDriverCreateInspectShutdownAndDelete(t *testing.T) {
	root := canonicalTempDir(t)
	providerRoot := filepath.Join(root, "provider")
	workspace := filepath.Join(root, "workspace")
	executable := filepath.Join(root, "pi")
	require.NoError(t, os.Mkdir(providerRoot, 0o700))
	require.NoError(t, os.Mkdir(workspace, 0o700))
	require.NoError(t, os.WriteFile(executable, []byte("binary"), 0o700))
	child := newRPCFakeChild()
	launcher := &recordingLauncher{child: child, requests: make(chan provider.LaunchRequest, 1)}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	driver, err := NewDriver(Config{Executable: executable, Environment: []string{"HOME=/isolated"}, ProviderRoot: providerRoot, Launcher: launcher, IDs: fixedIDs{value: behaviorID(80)}, Clock: fixedClock{value: now}})
	require.NoError(t, err)
	result := make(chan struct {
		session provider.Session
		err     error
	}, 1)
	go func() {
		session, createErr := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NamePi, Access: provider.AccessConfigured, Workspace: workspace})
		result <- struct {
			session provider.Session
			err     error
		}{session, createErr}
	}()
	launch := <-launcher.requests
	sessionFile := launch.Arguments[len(launch.Arguments)-1]
	writeStartupHeader(t, sessionFile, "native-session", workspace)
	command := child.readCommand(t)
	require.Equal(t, "get_state", command["type"])
	child.writeRecord(t, responseRecord(command, map[string]any{"model": map[string]any{"provider": "model-provider", "id": "model-id", "contextWindow": 32768, "maxTokens": 1024}, "isStreaming": false, "isCompacting": false, "pendingMessageCount": 0, "sessionFile": sessionFile, "sessionId": "native-session"}))
	created := <-result
	require.NoError(t, created.err)
	require.NoError(t, created.session.NativeSession().Validate())
	inspected, err := driver.Inspect(context.Background(), provider.InspectRequest{Provider: provider.NamePi, NativeSession: created.session.NativeSession().Ref})
	require.NoError(t, err)
	require.Equal(t, created.session.NativeSession(), inspected)
	err = driver.Delete(context.Background(), provider.DeleteRequest{Provider: provider.NamePi, NativeSession: inspected.Ref})
	assertProviderCode(t, err, provider.ErrorProtocolFailure)
	shutdown := make(chan error, 1)
	go func() { shutdown <- created.session.Shutdown(context.Background()) }()
	child.closeOutput()
	require.NoError(t, <-shutdown)
	require.NoError(t, driver.Delete(context.Background(), provider.DeleteRequest{Provider: provider.NamePi, NativeSession: inspected.Ref}))
}
