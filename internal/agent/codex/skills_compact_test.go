package codex

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

func TestParseSkillCatalogPublishesOnlyEnabledSafeMetadataAndPreservesIDs(t *testing.T) {
	workspace := "/workspace"
	raw := json.RawMessage(`{"data":[{"cwd":"/workspace","skills":[
		{"name":"review-helper","description":"Long description","shortDescription":"Legacy short","path":"/private/review/SKILL.md","scope":"repo","enabled":true,"interface":{"displayName":"Review helper","shortDescription":"Review this page","iconSmall":"https://example.invalid/icon.png","defaultPrompt":"secret"}},
		{"name":"disabled","description":"Hidden","path":"/private/disabled/SKILL.md","scope":"user","enabled":false,"interface":null}
	],"errors":[{"path":"/private/broken/SKILL.md","message":"private failure"}]}]}`)
	var generated atomic.Int32
	ids := func() (string, error) {
		generated.Add(1)
		return testID(uint64(900 + generated.Load())), nil
	}
	catalog, err := parseSkillCatalog(raw, workspace, nativeSkillCatalog{}, ids)
	require.NoError(t, err)
	safe := catalog.safeCatalog()
	require.Equal(t, provider.SkillsReady, safe.State)
	require.Len(t, safe.Skills, 1)
	require.Equal(t, provider.SkillDescriptor{ID: testID(901), Name: "review-helper", DisplayName: "Review helper", Description: "Review this page", Scope: provider.SkillScopeRepo}, safe.Skills[0])
	require.NotContains(t, string(mustJSON(t, safe)), "/private")
	require.NotContains(t, string(mustJSON(t, safe)), "secret")

	next, err := parseSkillCatalog(raw, workspace, catalog, ids)
	require.NoError(t, err)
	require.Equal(t, safe, next.safeCatalog())
	require.Equal(t, int32(1), generated.Load(), "unchanged scope/name/path identity keeps its opaque id")

	wrongWorkspace := strings.Replace(string(raw), `"cwd":"/workspace"`, `"cwd":"/other"`, 1)
	_, err = parseSkillCatalog(json.RawMessage(wrongWorkspace), workspace, catalog, ids)
	require.Error(t, err)
	duplicate := strings.Replace(string(raw), `{"name":"disabled"`, `{"name":"review-helper"`, 1)
	duplicate = strings.Replace(duplicate, `"enabled":false`, `"enabled":true`, 1)
	_, err = parseSkillCatalog(json.RawMessage(duplicate), workspace, catalog, ids)
	require.Error(t, err)
}

func TestSessionLoadsAndRefreshesWorkspaceSkillCatalogOutsideReadLoop(t *testing.T) {
	input := &recordingWriteCloser{writes: make(chan []byte, 4)}
	runtime := &runtime{input: input, pending: make(map[int64]chan rpcResult), barriers: make(map[int64]*responseBarrier), inbound: make(map[string]struct{}), sessions: make(map[string]*Session), done: make(chan struct{})}
	driver := &Driver{config: Config{Clock: fixedClock{time.Unix(100, 0).UTC()}, IDs: &sequenceIDs{}, IdleTimeout: time.Hour}, runtime: runtime}
	session := &Session{driver: driver, runtime: runtime, threadID: "native-thread", workspace: "/workspace", events: make(chan provider.Event, 8), view: newSessionChild(), skills: unavailableSkillCatalog(), activities: make(map[string]string), toolStates: make(map[string]provider.ToolActivity), interactions: make(map[string]nativeInteraction), supportsCompact: true}
	runtime.sessions[session.threadID] = session

	loaded := make(chan provider.SkillCatalog, 1)
	go func() { loaded <- session.Skills(context.Background()) }()
	request := <-input.writes
	require.Contains(t, string(request), `"method":"skills/list"`)
	require.Contains(t, string(request), `"cwds":["/workspace"]`)
	runtime.handleResponse(rpcEnvelope{ID: json.RawMessage(`1`), Result: skillListResult("review-helper")})
	catalog := <-loaded
	require.Equal(t, provider.SkillsReady, catalog.State)
	require.Len(t, catalog.Skills, 1)

	runtime.handleNotification("skills/changed", json.RawMessage(`{}`))
	refresh := <-input.writes
	require.Contains(t, string(refresh), `"method":"skills/list"`)
	runtime.handleResponse(rpcEnvelope{ID: json.RawMessage(`2`), Result: skillListResult("summarize")})
	event := awaitEvent(t, session.events)
	require.Equal(t, provider.EventSkillCatalog, event.Kind)
	require.Equal(t, "summarize", event.SkillCatalog.Skills[0].Name)
}

func TestExplicitSkillInputPrecedesEnvelopeAndImages(t *testing.T) {
	inputs, err := buildTurnInput("/workspace", []byte("envelope"), []nativeSkill{{name: "review-helper", path: "/private/review/SKILL.md"}}, nil)
	require.NoError(t, err)
	require.Equal(t, []turnInput{
		{Type: "skill", Name: "review-helper", Path: "/private/review/SKILL.md"},
		{Type: "text", Text: "envelope"},
	}, inputs)
	encoded := string(mustJSON(t, inputs))
	require.Contains(t, encoded, `"type":"skill"`)
	require.Contains(t, encoded, `"path":"/private/review/SKILL.md"`)
}

func TestSessionRejectsStaleSkillBeforeNativeTurnStart(t *testing.T) {
	input := newSignalingWriteCloser()
	runtime := &runtime{input: input, pending: make(map[int64]chan rpcResult), barriers: make(map[int64]*responseBarrier), inbound: make(map[string]struct{}), sessions: make(map[string]*Session), done: make(chan struct{})}
	runtime.catalog = testNativeCatalog(t)
	driver := &Driver{config: Config{Clock: fixedClock{time.Unix(100, 0).UTC()}, IDs: &sequenceIDs{}, IdleTimeout: time.Hour}, runtime: runtime}
	session := &Session{driver: driver, runtime: runtime, threadID: "native-thread", workspace: "/workspace", events: make(chan provider.Event, 8), view: newSessionChild(), activities: make(map[string]string), toolStates: make(map[string]provider.ToolActivity), interactions: make(map[string]nativeInteraction), supportsCompact: true}
	session.skills = nativeSkillCatalog{state: provider.SkillsReady, byID: map[string]nativeSkill{testID(910): {id: testID(910), name: "review-helper", path: "/private/review/SKILL.md", scope: provider.SkillScopeRepo}}, order: []string{testID(910)}}
	settings := defaultTestSettings()
	request := provider.TurnRequest{TurnID: testID(911), MessageID: testID(912), Content: provider.MessageContent{Parts: []provider.MessagePart{{Kind: provider.MessagePartSkill, Skill: &provider.SkillInvocation{ID: testID(913), Name: "removed"}}}}, Settings: &settings}
	_, err := session.Preflight(context.Background(), provider.PreflightRequest{Turn: request})
	assertProviderError(t, err, provider.ErrorSkillUnavailable)
	_, err = session.Submit(context.Background(), request)
	assertProviderError(t, err, provider.ErrorSkillUnavailable)
	select {
	case <-input.wrote:
		t.Fatal("stale skill reached native transport")
	default:
	}
}

func TestManualCompactCapturesDelayedNativeTurnAndInterruptsExactlyOnce(t *testing.T) {
	input := &recordingWriteCloser{writes: make(chan []byte, 4)}
	runtime := &runtime{input: input, pending: make(map[int64]chan rpcResult), barriers: make(map[int64]*responseBarrier), inbound: make(map[string]struct{}), sessions: make(map[string]*Session), done: make(chan struct{})}
	driver := &Driver{config: Config{Clock: fixedClock{time.Unix(100, 0).UTC()}, IDs: &sequenceIDs{}, IdleTimeout: time.Hour}, runtime: runtime}
	session := &Session{driver: driver, runtime: runtime, threadID: "native-thread", events: make(chan provider.Event, 8), view: newSessionChild(), activities: make(map[string]string), toolStates: make(map[string]provider.ToolActivity), interactions: make(map[string]nativeInteraction), supportsCompact: true}
	runtime.sessions[session.threadID] = session

	accepted := make(chan provider.AcceptedCompact, 1)
	compactErr := make(chan error, 1)
	go func() {
		value, err := session.Compact(context.Background(), provider.CompactRequest{WorkID: testID(920)})
		accepted <- value
		compactErr <- err
	}()
	compactRequest := <-input.writes
	require.Contains(t, string(compactRequest), `"method":"thread/compact/start"`)
	runtime.handleResponse(rpcEnvelope{ID: json.RawMessage(`1`), Result: json.RawMessage(`{}`)})
	require.NoError(t, <-compactErr)
	value := <-accepted
	require.Equal(t, testID(920), value.WorkID)

	require.NoError(t, session.InterruptCompact(context.Background(), value), "stop is deferred until native correlation")
	session.handleNotification("item/started", mustJSON(t, map[string]any{"threadId": "native-thread", "turnId": "native-compact", "item": map[string]any{"id": "compact-item", "type": "contextCompaction", "status": "inProgress"}}))
	interruptRequest := <-input.writes
	require.Contains(t, string(interruptRequest), `"method":"turn/interrupt"`)
	runtime.handleResponse(rpcEnvelope{ID: json.RawMessage(`2`), Result: json.RawMessage(`{}`)})
	require.NoError(t, session.InterruptCompact(context.Background(), value), "duplicate stop is idempotent at the adapter boundary")
	select {
	case <-input.writes:
		t.Fatal("duplicate compact interrupt reached native transport")
	case <-time.After(20 * time.Millisecond):
	}

	session.handleNotification("turn/completed", mustJSON(t, map[string]any{"threadId": "native-thread", "turn": map[string]any{"id": "native-compact", "status": "interrupted"}}))
	event := awaitEvent(t, session.events)
	require.Equal(t, provider.EventCompact, event.Kind)
	require.Equal(t, provider.CompactInterrupted, event.Compact.Status)
}

func TestManualCompactTerminalStatesAndUnsupportedRuntime(t *testing.T) {
	for nativeStatus, expected := range map[string]provider.CompactStatus{
		"completed":   provider.CompactCompleted,
		"interrupted": provider.CompactInterrupted,
		"failed":      provider.CompactFailed,
	} {
		t.Run(nativeStatus, func(t *testing.T) {
			session := &Session{events: make(chan provider.Event, 1), compact: &nativeCompact{request: provider.CompactRequest{WorkID: testID(930)}, accepted: true, nativeID: "native-compact"}}
			turn := map[string]any{"id": "native-compact", "status": nativeStatus}
			if nativeStatus == "failed" {
				turn["error"] = map[string]any{"message": "failed"}
			}
			session.handleNotification("turn/completed", mustJSON(t, map[string]any{"threadId": "native-thread", "turn": turn}))
			event := awaitEvent(t, session.events)
			require.Equal(t, expected, event.Compact.Status)
		})
	}

	input := &recordingWriteCloser{writes: make(chan []byte, 2)}
	runtime := &runtime{input: input, pending: make(map[int64]chan rpcResult), barriers: make(map[int64]*responseBarrier), done: make(chan struct{})}
	driver := &Driver{config: Config{Clock: fixedClock{time.Unix(100, 0).UTC()}}}
	session := &Session{driver: driver, runtime: runtime, threadID: "native-thread", supportsCompact: true}
	result := make(chan error, 1)
	go func() {
		_, err := session.Compact(context.Background(), provider.CompactRequest{WorkID: testID(931)})
		result <- err
	}()
	<-input.writes
	runtime.handleResponse(rpcEnvelope{ID: json.RawMessage(`1`), Error: &rpcError{Code: -32601, Message: "method not found"}})
	assertProviderError(t, <-result, provider.ErrorCompactUnsupported)
	require.False(t, session.SupportsCompact())
	second := &Session{runtime: runtime, supportsCompact: true}
	require.False(t, second.SupportsCompact(), "unsupported compact capability is shared across runtime sessions")
}

func TestManualCompactDoesNotConsumeUnrelatedSettingsNotifications(t *testing.T) {
	settings := provider.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "high", Speed: provider.SpeedStandard}
	presentation := provider.ModelPresentation{ModelDisplayName: "5.6 Sol", Selectable: true}
	session := &Session{
		native: provider.NativeSession{Provider: provider.NameCodex, Model: settings.Model, Settings: &settings, Presentation: &presentation},
		events: make(chan provider.Event, 2), compact: &nativeCompact{request: provider.CompactRequest{WorkID: testID(932)}, accepted: true, nativeID: "native-compact"},
		catalog:  nativeCatalog{models: map[string]nativeModelRecord{"gpt-5.6-luna": {Model: "gpt-5.6-luna", DisplayName: "5.6 Luna", DefaultEffort: "medium", Efforts: []provider.ReasoningEffort{{Value: "medium", Description: "Balanced"}}, ServiceTiers: []nativeServiceTier{{ID: "default", Name: "Default"}}}}, aliases: map[string]string{}},
		threadID: "native-thread", driver: &Driver{config: Config{Clock: fixedClock{time.Unix(100, 0).UTC()}}},
	}
	session.handleNotification("thread/settings/updated", mustJSON(t, map[string]any{"threadId": "native-thread", "threadSettings": map[string]any{"model": "gpt-5.6-luna", "effort": "medium", "serviceTier": nil}}))
	event := awaitEvent(t, session.events)
	require.Equal(t, provider.EventSettings, event.Kind)
	require.Equal(t, "gpt-5.6-luna", event.Settings.Model)
	require.NotNil(t, session.compact)
}

func skillListResult(name string) json.RawMessage {
	return json.RawMessage(`{"data":[{"cwd":"/workspace","skills":[{"name":"` + name + `","description":"Description","path":"/private/` + name + `/SKILL.md","scope":"repo","enabled":true,"interface":null}],"errors":[]}]}`)
}

type recordingWriteCloser struct{ writes chan []byte }

func (writer *recordingWriteCloser) Write(value []byte) (int, error) {
	writer.writes <- append([]byte(nil), value...)
	return len(value), nil
}
func (*recordingWriteCloser) Close() error { return nil }
