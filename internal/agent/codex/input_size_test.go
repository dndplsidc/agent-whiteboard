package codex

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

func TestInputTooLargeClassification(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure rpcError
		want    provider.ProviderErrorCode
	}{
		{"captured rejection", rpcError{Code: -32602, Message: "Input exceeds the maximum length of 1048576 characters.", Data: json.RawMessage(`{"input_error_code":"input_too_large","max_chars":1048576,"actual_chars":1705318}`)}, provider.ErrorContextTooLarge},
		{"structured code without English message", rpcError{Code: -32602, Message: "Rejected", Data: json.RawMessage(`{"input_error_code":"input_too_large"}`)}, provider.ErrorContextTooLarge},
		{"unrelated invalid input", rpcError{Code: -32602, Message: "Invalid input", Data: json.RawMessage(`{"input_error_code":"invalid_image"}`)}, provider.ErrorProtocolFailure},
		{"code mentioned only in text", rpcError{Code: -32602, Message: "Unknown value input_too_large"}, provider.ErrorProtocolFailure},
		{"malformed data", rpcError{Code: -32602, Data: json.RawMessage(`{"input_error_code":`)}, provider.ErrorProtocolFailure},
	} {
		t.Run(tc.name, func(t *testing.T) { assertProviderError(t, classifyRPCError(&tc.failure), tc.want) })
	}
}

// Exercise the JSON-RPC boundary and ensure a definite rejection releases the
// active turn so a reader can send a smaller request in the same session.
func TestSubmitInputTooLargeAllowsSmallerTurn(t *testing.T) {
	attempts := 0
	launcher := readyLauncher(t, func(child *scriptedChild, request map[string]json.RawMessage, method string) {
		switch method {
		case "thread/start":
			child.send(t, map[string]any{"id": request["id"], "result": completeThreadResponse("size-thread", "gpt-fixture", "medium", nil)})
		case "turn/start":
			attempts++
			if attempts == 1 {
				child.send(t, map[string]any{"id": request["id"], "error": map[string]any{"code": -32602, "message": "Input exceeds the maximum length of 1048576 characters.", "data": map[string]any{"input_error_code": "input_too_large", "max_chars": 1048576, "actual_chars": 1705318}}})
			} else {
				child.send(t, map[string]any{"id": request["id"], "result": map[string]any{"turn": map[string]any{"id": "smaller-turn"}}})
				child.send(t, notification("turn/completed", map[string]any{"threadId": "size-thread", "turn": map[string]any{"id": "smaller-turn", "status": "completed"}}))
			}
		default:
			t.Errorf("unexpected operation %s", method)
		}
	})
	driver, err := NewDriver(Config{Executable: "/fixture/bin/codex", Environment: []string{"PATH=/fixture/bin"}, ProviderRoot: t.TempDir(), Launcher: launcher, IDs: &sequenceIDs{}, Clock: fixedClock{time.Unix(100, 0).UTC()}, IdleTimeout: time.Hour})
	require.NoError(t, err)
	created, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCodex, Access: provider.AccessConfigured, Workspace: "/workspace"})
	require.NoError(t, err)
	session := created.(*Session)
	t.Cleanup(func() { session.runtime.close(); _ = session.Shutdown(context.Background()) })
	settings := defaultTestSettings()
	request := provider.TurnRequest{TurnID: testID(910), MessageID: testID(911), Content: provider.TextMessage("Explain the page"), Settings: &settings}
	_, err = session.Submit(context.Background(), request)
	assertProviderError(t, err, provider.ErrorContextTooLarge)
	request.TurnID, request.MessageID = testID(912), testID(913)
	request.Content = provider.TextMessage("Explain this section")
	accepted, err := session.Submit(context.Background(), request)
	require.NoError(t, err)
	require.Equal(t, request.TurnID, accepted.TurnID)
	for event := awaitEvent(t, session.Events()); event.Kind != provider.EventCompletion; event = awaitEvent(t, session.Events()) {
	}
	require.Equal(t, 2, attempts, "the rejected turn must not be replayed")
}
