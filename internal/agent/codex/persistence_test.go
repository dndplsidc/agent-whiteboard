package codex

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

func TestCreatePersistsEmptyThreadBeforeReturningAndResumesAfterRuntimeRestart(t *testing.T) {
	persisted := false
	legacy := false
	read := false
	launcher := readyLauncherWithPersistence(t, func(child *scriptedChild, request map[string]json.RawMessage, method string) {
		var params map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(request["params"], &params))
		switch method {
		case "thread/start":
			legacy = string(params["historyMode"]) == `"legacy"`
			child.send(t, map[string]any{"id": request["id"], "result": completeThreadResponse("durable-empty", "gpt-fixture", "medium", nil)})
		case "thread/name/set":
			require.JSONEq(t, `"Page Agent"`, string(params["name"]))
			require.JSONEq(t, `"durable-empty"`, string(params["threadId"]))
			persisted = legacy
			child.send(t, map[string]any{"id": request["id"], "result": map[string]any{}})
		case "thread/read":
			read = persisted && string(params["includeTurns"]) == "true"
			child.send(t, map[string]any{"id": request["id"], "result": map[string]any{"thread": map[string]any{"id": "durable-empty", "turns": []any{}}}})
		case "thread/resume":
			if !persisted {
				child.send(t, map[string]any{"id": request["id"], "error": map[string]any{"code": -32600, "message": "no rollout found for thread id durable-empty"}})
			} else {
				child.send(t, map[string]any{"id": request["id"], "result": completeThreadResponse("durable-empty", "gpt-fixture", "medium", nil)})
			}
		default:
			t.Errorf("unexpected native operation %s (no synthetic turns allowed)", method)
		}
	}, false)
	driver, err := NewDriver(Config{Executable: "/fixture/bin/codex", Environment: []string{"PATH=/fixture/bin"}, ProviderRoot: t.TempDir(), Launcher: launcher, IDs: &sequenceIDs{}, Clock: fixedClock{time.Unix(100, 0).UTC()}, IdleTimeout: time.Hour})
	require.NoError(t, err)
	created, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCodex, Access: provider.AccessConfigured, Workspace: "/workspace"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = created.Shutdown(context.Background()); created.(*Session).runtime.close() })
	require.True(t, persisted, "creation must materialize legacy history before exposing its native ID")
	require.True(t, read, "creation must verify full history is readable")
	ref := created.NativeSession().Ref
	require.NoError(t, created.Shutdown(context.Background()))
	created.(*Session).runtime.close()
	resumed, err := driver.Resume(context.Background(), provider.ResumeRequest{Provider: provider.NameCodex, Access: provider.AccessConfigured, Workspace: "/workspace", NativeSession: ref})
	require.NoError(t, err)
	t.Cleanup(func() { _ = resumed.Shutdown(context.Background()); resumed.(*Session).runtime.close() })
	require.Equal(t, ref, resumed.NativeSession().Ref)
	require.Len(t, launcher.requests, 2)
}

func TestCreateRetainsCleanupHandleWhenPersistenceCannotBeVerified(t *testing.T) {
	for _, mode := range []string{"name unsupported", "name failure", "unmaterialized", "wrong thread", "missing turns"} {
		t.Run(mode, func(t *testing.T) {
			launcher := readyLauncherWithPersistence(t, func(child *scriptedChild, request map[string]json.RawMessage, method string) {
				response := map[string]any{"id": request["id"]}
				switch method {
				case "thread/start":
					response["result"] = completeThreadResponse("empty-thread", "gpt-fixture", "medium", nil)
				case "thread/name/set":
					if mode == "name unsupported" {
						response["error"] = map[string]any{"code": -32601, "message": "method not found"}
					} else if mode == "name failure" {
						response["error"] = map[string]any{"code": -32603, "message": "write failed"}
					} else {
						response["result"] = map[string]any{}
					}
				case "thread/read":
					switch mode {
					case "unmaterialized":
						response["error"] = map[string]any{"code": -32600, "message": "thread empty-thread is not materialized yet; includeTurns is unavailable before first user message"}
					case "wrong thread":
						response["result"] = map[string]any{"thread": map[string]any{"id": "other-thread", "turns": []any{}}}
					case "missing turns":
						response["result"] = map[string]any{"thread": map[string]any{"id": "empty-thread"}}
					}
				default:
					t.Errorf("unexpected operation %s", method)
				}
				child.send(t, response)
			}, false)
			driver, err := NewDriver(Config{Executable: "/fixture/bin/codex", Environment: []string{"PATH=/fixture/bin"}, ProviderRoot: t.TempDir(), Launcher: launcher, IDs: &sequenceIDs{}, Clock: fixedClock{time.Unix(100, 0).UTC()}, IdleTimeout: time.Hour})
			require.NoError(t, err)
			created, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCodex, Access: provider.AccessConfigured, Workspace: "/workspace"})
			require.Error(t, err)
			require.NotNil(t, created, "broker must retain ownership of failed native candidate cleanup")
			require.NoError(t, created.Shutdown(context.Background()))
			created.(*Session).runtime.close()
		})
	}
}

func TestMissingThreadErrorSpellings(t *testing.T) {
	for _, message := range []string{"thread not found", "threadNotFound", "thread_not_found", "no rollout found for thread id missing"} {
		assertProviderError(t, classifyRPCError(&rpcError{Code: -32600, Message: message}), provider.ErrorNativeSessionMissing)
	}
	assertProviderError(t, classifyRPCError(&rpcError{Code: -32603, Message: "rollout read failed: permission denied"}), provider.ErrorProtocolFailure)
}
