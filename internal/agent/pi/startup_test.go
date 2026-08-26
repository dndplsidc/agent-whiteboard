package pi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return path
}

func writeStartupHeader(t *testing.T, path, sessionID, workspace string) {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"type": "session", "version": 3, "id": sessionID, "timestamp": "2026-01-02T03:04:05Z", "cwd": workspace})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(encoded, '\n'), 0o600))
}

func TestStartupCorrelatesOutOfOrderResponsesAndValidatesDisk(t *testing.T) {
	root := canonicalTempDir(t)
	workspace := filepath.Join(root, "workspace")
	require.NoError(t, os.Mkdir(workspace, 0o700))
	sessionFile := filepath.Join(root, "session.jsonl")
	writeStartupHeader(t, sessionFile, "session-id", workspace)

	child := newRPCFakeChild()
	client, err := newRPCClient(child)
	require.NoError(t, err)
	answer := make(chan struct {
		state startupState
		err   error
	}, 1)
	go func() {
		state, startupErr := startup(context.Background(), client, sessionFile, workspace)
		answer <- struct {
			state startupState
			err   error
		}{state, startupErr}
	}()
	command := child.readCommand(t)
	require.Equal(t, "get_state", command["type"])
	child.writeRecord(t, map[string]any{"id": command["id"], "type": "response", "command": command["type"], "success": true, "data": map[string]any{
		"model":       map[string]any{"provider": "model-provider", "id": "model-id", "contextWindow": 32768, "maxTokens": 1024, "future": true},
		"isStreaming": false, "isCompacting": false, "pendingMessageCount": 0,
		"sessionFile": sessionFile, "sessionId": "session-id", "future": true,
	}})
	resolved := <-answer
	require.NoError(t, resolved.err)
	require.Equal(t, "model-provider/model-id", resolved.state.Model)
	require.Equal(t, 32768, resolved.state.ContextWindow)
	child.closeOutput()
}

func TestStartupClosedRPCIsStartupFailure(t *testing.T) {
	root := canonicalTempDir(t)
	workspace := filepath.Join(root, "workspace")
	require.NoError(t, os.Mkdir(workspace, 0o700))
	sessionFile := filepath.Join(root, "session.jsonl")
	writeStartupHeader(t, sessionFile, "session-id", workspace)
	child := newRPCFakeChild()
	client, err := newRPCClient(child)
	require.NoError(t, err)
	child.closeOutput()
	<-client.done
	_, err = startup(context.Background(), client, sessionFile, workspace)
	assertProviderCode(t, err, provider.ErrorStartupFailed)
}

func TestStartupIgnoresBufferedActivityEvents(t *testing.T) {
	root := canonicalTempDir(t)
	workspace := filepath.Join(root, "workspace")
	require.NoError(t, os.Mkdir(workspace, 0o700))
	sessionFile := filepath.Join(root, "session.jsonl")
	writeStartupHeader(t, sessionFile, "session-id", workspace)
	child := newRPCFakeChild()
	client, err := newRPCClient(child)
	require.NoError(t, err)
	answer := make(chan struct {
		state startupState
		err   error
	}, 1)
	go func() {
		state, startupErr := startup(context.Background(), client, sessionFile, workspace)
		answer <- struct {
			state startupState
			err   error
		}{state, startupErr}
	}()
	command := child.readCommand(t)
	child.writeRecord(t, map[string]any{"type": "tool_execution_start", "secret": "/private/token"})
	child.writeRecord(t, map[string]any{"id": command["id"], "type": "response", "command": command["type"], "success": true, "data": map[string]any{
		"model":       map[string]any{"provider": "model-provider", "id": "model-id", "contextWindow": 32768, "maxTokens": 1024},
		"isStreaming": false, "isCompacting": false, "pendingMessageCount": 0,
		"sessionFile": sessionFile, "sessionId": "session-id",
	}})
	resolved := <-answer
	require.NoError(t, resolved.err)
	require.Equal(t, "model-provider/model-id", resolved.state.Model)
	child.closeOutput()
}

func TestStartupRejectsMalformedStateWrongIdentityAndNoModel(t *testing.T) {
	tests := []struct {
		name  string
		model any
		file  string
		code  provider.ProviderErrorCode
	}{
		{name: "no model", model: nil, code: provider.ErrorNoUsableModel},
		{name: "malformed model", model: map[string]any{"provider": "p", "id": "m", "contextWindow": 0, "maxTokens": 1}, code: provider.ErrorProtocolIncompatible},
		{name: "wrong path", model: map[string]any{"provider": "p", "id": "m", "contextWindow": 10, "maxTokens": 1}, file: "/wrong/session.jsonl", code: provider.ErrorProtocolIncompatible},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := canonicalTempDir(t)
			workspace := filepath.Join(root, "workspace")
			require.NoError(t, os.Mkdir(workspace, 0o700))
			sessionFile := filepath.Join(root, "session.jsonl")
			writeStartupHeader(t, sessionFile, "session-id", workspace)
			child := newRPCFakeChild()
			client, err := newRPCClient(child)
			require.NoError(t, err)
			answer := make(chan error, 1)
			go func() {
				_, startupErr := startup(context.Background(), client, sessionFile, workspace)
				answer <- startupErr
			}()
			command := child.readCommand(t)
			returnedFile := sessionFile
			if test.file != "" {
				returnedFile = test.file
			}
			child.writeRecord(t, map[string]any{"id": command["id"], "type": "response", "command": command["type"], "success": true, "data": map[string]any{"model": test.model, "isStreaming": false, "isCompacting": false, "pendingMessageCount": 0, "sessionFile": returnedFile, "sessionId": "session-id"}})
			assertProviderCode(t, <-answer, test.code)
			child.closeOutput()
		})
	}
}

func TestStartupRejectsNonIdlePendingWrongSessionAndCommands(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
		code   provider.ProviderErrorCode
	}{
		{name: "streaming", mutate: func(state map[string]any) { state["isStreaming"] = true }, code: provider.ErrorStartupFailed},
		{name: "compacting", mutate: func(state map[string]any) { state["isCompacting"] = true }, code: provider.ErrorStartupFailed},
		{name: "pending", mutate: func(state map[string]any) { state["pendingMessageCount"] = 1 }, code: provider.ErrorStartupFailed},
		{name: "wrong session id", mutate: func(state map[string]any) { state["sessionId"] = "wrong" }, code: provider.ErrorNativeSessionMissing},
		{name: "missing required state", mutate: func(state map[string]any) { delete(state, "isStreaming") }, code: provider.ErrorProtocolIncompatible},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := canonicalTempDir(t)
			workspace := filepath.Join(root, "workspace")
			require.NoError(t, os.Mkdir(workspace, 0o700))
			sessionFile := filepath.Join(root, "session.jsonl")
			writeStartupHeader(t, sessionFile, "session-id", workspace)
			stateData := map[string]any{
				"model":       map[string]any{"provider": "p", "id": "m", "contextWindow": 10, "maxTokens": 1},
				"isStreaming": false, "isCompacting": false, "pendingMessageCount": 0,
				"sessionFile": sessionFile, "sessionId": "session-id",
			}
			if test.mutate != nil {
				test.mutate(stateData)
			}
			child := newRPCFakeChild()
			client, err := newRPCClient(child)
			require.NoError(t, err)
			answer := make(chan error, 1)
			go func() {
				_, startupErr := startup(context.Background(), client, sessionFile, workspace)
				answer <- startupErr
			}()
			command := child.readCommand(t)
			child.writeRecord(t, map[string]any{"id": command["id"], "type": "response", "command": command["type"], "success": true, "data": stateData})
			assertProviderCode(t, <-answer, test.code)
			child.closeOutput()
		})
	}
}
