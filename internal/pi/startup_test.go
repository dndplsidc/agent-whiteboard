package pi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/edocsss/agent-whiteboard/internal/provider"
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
	commands := []map[string]any{child.readCommand(t), child.readCommand(t)}
	for index := len(commands) - 1; index >= 0; index-- {
		command := commands[index]
		data := any(map[string]any{"commands": []any{map[string]any{"name": "llama", "source": "extension", "description": "bundled", "future": true}}, "future": true})
		if command["type"] == "get_state" {
			data = map[string]any{
				"model":       map[string]any{"provider": "model-provider", "id": "model-id", "contextWindow": 32768, "maxTokens": 1024, "future": true},
				"isStreaming": false, "isCompacting": false, "pendingMessageCount": 0,
				"sessionFile": sessionFile, "sessionId": "session-id", "future": true,
			}
		}
		child.writeRecord(t, map[string]any{"id": command["id"], "type": "response", "command": command["type"], "success": true, "data": data})
	}
	autoRetry := child.readCommand(t)
	require.Equal(t, "set_auto_retry", autoRetry["type"])
	require.Equal(t, false, autoRetry["enabled"])
	child.writeRecord(t, map[string]any{"id": autoRetry["id"], "type": "response", "command": "set_auto_retry", "success": true})
	resolved := <-answer
	require.NoError(t, resolved.err)
	require.Equal(t, "model-provider/model-id", resolved.state.Model)
	require.Equal(t, 32768, resolved.state.ContextWindow)
	child.closeOutput()
}

func TestContentOnlyCommandsAllowsOnlyEmptyOrBundledRPCLlama(t *testing.T) {
	require.True(t, contentOnlyCommands([]startupCommand{}))
	require.True(t, contentOnlyCommands([]startupCommand{{Name: "llama", Source: "extension"}}))
	require.False(t, contentOnlyCommands(nil))
	require.False(t, contentOnlyCommands([]startupCommand{{Name: "llama", Source: "prompt"}}))
	require.False(t, contentOnlyCommands([]startupCommand{{Name: "other", Source: "extension"}}))
}

func TestStartupClosedRPCIsStartupFailureNotAuthorityViolation(t *testing.T) {
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

func TestStartupRejectsAuthorityEventsWithoutSurfacingPayload(t *testing.T) {
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
	child.readCommand(t)
	child.readCommand(t)
	child.writeRecord(t, map[string]any{"type": "tool_execution_start", "secret": "/private/token"})
	err = <-answer
	assertProviderCode(t, err, provider.ErrorContentOnlyUnavailable)
	require.NotContains(t, err.Error(), "token")
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
			commands := []map[string]any{child.readCommand(t), child.readCommand(t)}
			for _, command := range commands {
				data := any(map[string]any{"commands": []any{}})
				if command["type"] == "get_state" {
					returnedFile := sessionFile
					if test.file != "" {
						returnedFile = test.file
					}
					data = map[string]any{"model": test.model, "isStreaming": false, "isCompacting": false, "pendingMessageCount": 0, "sessionFile": returnedFile, "sessionId": "session-id"}
				}
				child.writeRecord(t, map[string]any{"id": command["id"], "type": "response", "command": command["type"], "success": true, "data": data})
			}
			autoRetry := child.readCommand(t)
			child.writeRecord(t, map[string]any{"id": autoRetry["id"], "type": "response", "command": "set_auto_retry", "success": true})
			assertProviderCode(t, <-answer, test.code)
			child.closeOutput()
		})
	}
}

func TestStartupRejectsNonIdlePendingWrongSessionAndCommands(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(map[string]any)
		commands []any
		code     provider.ProviderErrorCode
	}{
		{name: "streaming", mutate: func(state map[string]any) { state["isStreaming"] = true }, code: provider.ErrorStartupFailed},
		{name: "compacting", mutate: func(state map[string]any) { state["isCompacting"] = true }, code: provider.ErrorStartupFailed},
		{name: "pending", mutate: func(state map[string]any) { state["pendingMessageCount"] = 1 }, code: provider.ErrorStartupFailed},
		{name: "wrong session id", mutate: func(state map[string]any) { state["sessionId"] = "wrong" }, code: provider.ErrorNativeSessionMissing},
		{name: "missing required state", mutate: func(state map[string]any) { delete(state, "isStreaming") }, code: provider.ErrorProtocolIncompatible},
		{name: "nonempty commands", commands: []any{map[string]any{"name": "tool"}}, code: provider.ErrorContentOnlyUnavailable},
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
			commandsData := test.commands
			if commandsData == nil {
				commandsData = []any{}
			}
			child := newRPCFakeChild()
			client, err := newRPCClient(child)
			require.NoError(t, err)
			answer := make(chan error, 1)
			go func() {
				_, startupErr := startup(context.Background(), client, sessionFile, workspace)
				answer <- startupErr
			}()
			for range 2 {
				command := child.readCommand(t)
				data := any(map[string]any{"commands": commandsData})
				if command["type"] == "get_state" {
					data = stateData
				}
				child.writeRecord(t, map[string]any{"id": command["id"], "type": "response", "command": command["type"], "success": true, "data": data})
			}
			autoRetry := child.readCommand(t)
			child.writeRecord(t, map[string]any{"id": autoRetry["id"], "type": "response", "command": "set_auto_retry", "success": true})
			assertProviderCode(t, <-answer, test.code)
			child.closeOutput()
		})
	}
}
