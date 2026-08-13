package codex

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

func TestNativeCatalogPreservesVisibleModelsEffortsCapabilitiesAndFastSupport(t *testing.T) {
	page, cursor, err := parseModelCatalogPage([]byte(`{
		"data":[
			{"id":"sol-alias","model":"gpt-5.6-sol","displayName":"5.6 Sol","description":"Deep coding","hidden":false,"isDefault":true,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"low","description":"Quick"},{"reasoningEffort":"medium","description":"Balanced"},{"reasoningEffort":"high","description":"Deep"}],"inputModalities":["text","image"],"serviceTiers":[{"id":"priority","name":"Fast","description":"Priority processing"}]},
			{"id":"hidden-alias","model":"gpt-hidden","displayName":"Hidden","description":"Hidden current model","hidden":true,"isDefault":false,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium","description":"Balanced"}],"inputModalities":["text"],"serviceTiers":[]}
		],
		"nextCursor":"next-page"
	}`))
	require.NoError(t, err)
	require.Equal(t, "next-page", cursor)
	require.Len(t, page, 2)

	catalog, err := newNativeCatalog(page)
	require.NoError(t, err)
	visible := catalog.visibleCatalog()
	require.NoError(t, visible.Validate())
	require.Len(t, visible.Models, 1)
	model := visible.Models[0]
	require.Equal(t, "gpt-5.6-sol", model.Model)
	require.Equal(t, "5.6 Sol", model.DisplayName)
	require.Equal(t, []provider.ReasoningEffort{{Value: "low", Description: "Quick"}, {Value: "medium", Description: "Balanced"}, {Value: "high", Description: "Deep"}}, model.SupportedReasoningEfforts)
	require.True(t, model.SupportsImages)
	require.True(t, model.SupportsFast)

	canonical, native, presentation, capabilities, err := catalog.resolveSubmitted(provider.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "high", Speed: provider.SpeedFast})
	require.NoError(t, err)
	require.Equal(t, "gpt-5.6-sol", canonical.Model)
	require.Equal(t, "gpt-5.6-sol", native)
	require.Equal(t, provider.ModelPresentation{ModelDisplayName: "5.6 Sol", Selectable: true}, presentation)
	require.True(t, capabilities.Images)

	effective, hiddenPresentation, hiddenCapabilities, err := catalog.resolveEffective("hidden-alias", "medium", "default")
	require.NoError(t, err)
	require.Equal(t, provider.ExecutionSettings{Model: "gpt-hidden", Effort: "medium", Speed: provider.SpeedStandard}, effective)
	require.Equal(t, provider.ModelPresentation{ModelDisplayName: "Hidden", Selectable: false}, hiddenPresentation)
	require.False(t, hiddenCapabilities.Images)

	removed, removedPresentation, _, err := catalog.resolveEffective("gpt-removed", "high", "priority")
	require.NoError(t, err)
	require.Equal(t, provider.ExecutionSettings{Model: "gpt-removed", Effort: "high", Speed: provider.SpeedFast}, removed)
	require.Equal(t, provider.ModelPresentation{ModelDisplayName: "gpt-removed", Selectable: false}, removedPresentation)
}

func TestNativeCatalogRejectsMalformedDuplicateAndUnsupportedStructures(t *testing.T) {
	valid := `{"data":[{"id":"sol","model":"sol","displayName":"Sol","description":"desc","hidden":false,"isDefault":true,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium","description":"Balanced"}],"inputModalities":["text"],"serviceTiers":[]}],"nextCursor":null}`
	for name, input := range map[string]string{
		"missing display":            `{"data":[{"id":"sol","model":"sol","description":"desc","hidden":false,"isDefault":true,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium","description":"Balanced"}]}],"nextCursor":null}`,
		"missing description":        `{"data":[{"id":"sol","model":"sol","displayName":"Sol","hidden":false,"isDefault":true,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium","description":"Balanced"}]}],"nextCursor":null}`,
		"missing effort description": `{"data":[{"id":"sol","model":"sol","displayName":"Sol","description":"desc","hidden":false,"isDefault":true,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium"}]}],"nextCursor":null}`,
		"missing tier description":   `{"data":[{"id":"sol","model":"sol","displayName":"Sol","description":"desc","hidden":false,"isDefault":true,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium","description":"Balanced"}],"serviceTiers":[{"id":"priority","name":"Fast"}]}],"nextCursor":null}`,
		"duplicate effort":           `{"data":[{"id":"sol","model":"sol","displayName":"Sol","description":"desc","hidden":false,"isDefault":true,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium","description":"one"},{"reasoningEffort":"medium","description":"two"}]}],"nextCursor":null}`,
		"unknown modality":           `{"data":[{"id":"sol","model":"sol","displayName":"Sol","description":"desc","hidden":false,"isDefault":true,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium","description":"Balanced"}],"inputModalities":["video"]}],"nextCursor":null}`,
		"duplicate tier":             `{"data":[{"id":"sol","model":"sol","displayName":"Sol","description":"desc","hidden":false,"isDefault":true,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium","description":"Balanced"}],"serviceTiers":[{"id":"priority","name":"Fast","description":"one"},{"id":"priority","name":"Fast","description":"two"}]}],"nextCursor":null}`,
		"empty cursor":               `{"data":[],"nextCursor":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := parseModelCatalogPage([]byte(input))
			require.Error(t, err)
		})
	}
	page, _, err := parseModelCatalogPage([]byte(valid))
	require.NoError(t, err)
	_, err = newNativeCatalog(append(page, page[0]))
	require.Error(t, err)
}

func TestModelCatalogPaginationRejectsRepeatedCursor(t *testing.T) {
	runtime, requests, child := pipeRuntime(t)
	go runtime.readLoop(child.Output())
	result := make(chan error, 1)
	go func() {
		_, err := loadModelCatalog(context.Background(), runtime)
		result <- err
	}()
	for index := 0; index < 2; index++ {
		request := <-requests
		var page map[string]any
		require.NoError(t, json.Unmarshal([]byte(testModelListJSON()), &page))
		page["nextCursor"] = "same-cursor"
		child.send(t, map[string]any{"id": request["id"], "result": page})
	}
	require.Error(t, <-result)
	child.stop()
}

func TestDriverAppliesExactCreateAndTurnSettingsOnOneNativeThread(t *testing.T) {
	launcher := readyLauncher(t, func(child *scriptedChild, request map[string]json.RawMessage, method string) {
		switch method {
		case "thread/start":
			var params map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(request["params"], &params))
			require.Equal(t, []string{"config", "cwd", "model", "serviceTier"}, sortedKeys(params))
			require.JSONEq(t, `{"model_reasoning_effort":"high"}`, string(params["config"]))
			require.JSONEq(t, `"gpt-5.6-sol"`, string(params["model"]))
			require.JSONEq(t, `"priority"`, string(params["serviceTier"]))
			child.send(t, map[string]any{"id": request["id"], "result": completeThreadResponse("native-thread", "gpt-5.6-sol", "high", "priority")})
		case "turn/start":
			var params map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(request["params"], &params))
			require.JSONEq(t, `"native-thread"`, string(params["threadId"]))
			require.JSONEq(t, `"gpt-5.6-luna"`, string(params["model"]))
			require.JSONEq(t, `"medium"`, string(params["effort"]))
			require.JSONEq(t, `null`, string(params["serviceTier"]))
			child.send(t, map[string]any{"id": request["id"], "result": map[string]any{"turn": map[string]any{"id": "native-turn"}}})
		case "turn/interrupt":
			child.send(t, map[string]any{"id": request["id"], "result": map[string]any{}})
		}
	})
	driver, err := NewDriver(Config{Executable: "/fixture/bin/codex", Environment: []string{"PATH=/fixture/bin"}, ProviderRoot: t.TempDir(), Launcher: launcher, IDs: &sequenceIDs{}, Clock: fixedClock{time.Unix(100, 0).UTC()}, IdleTimeout: time.Hour})
	require.NoError(t, err)

	catalog, err := driver.ModelCatalog(context.Background())
	require.NoError(t, err)
	require.Len(t, catalog.Models, 3)
	initial := provider.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "high", Speed: provider.SpeedFast}
	created, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCodex, Access: provider.AccessConfigured, Workspace: "/workspace", Settings: &initial})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, created.Shutdown(context.Background())) })
	require.Equal(t, initial, *created.NativeSession().Settings)

	next := provider.ExecutionSettings{Model: "gpt-5.6-luna", Effort: "medium", Speed: provider.SpeedStandard}
	accepted, err := created.Submit(context.Background(), provider.TurnRequest{TurnID: testID(940), MessageID: testID(941), Content: provider.TextMessage("switch"), Settings: &next})
	require.NoError(t, err)
	require.Equal(t, next, *accepted.Settings)
	require.Equal(t, "native-thread", created.NativeSession().Ref.Value())
	require.Equal(t, next, *created.NativeSession().Settings)
}

func TestPreflightUsesCapturedModelImageCapability(t *testing.T) {
	catalog := testNativeCatalog(t)
	initial, presentation, capabilities, err := catalog.resolveEffective("gpt-5.6-sol", "high", "priority")
	require.NoError(t, err)
	session := &Session{native: testCodexNativeSession(t, "native-thread", initial, presentation), capabilities: capabilities, catalog: catalog}
	luna := provider.ExecutionSettings{Model: "gpt-5.6-luna", Effort: "medium", Speed: provider.SpeedStandard}
	request := provider.PreflightRequest{Turn: provider.TurnRequest{
		TurnID: testID(942), MessageID: testID(943), Content: provider.TextMessage("inspect"), Settings: &luna,
		Images: []provider.ImageInput{{ID: testID(944), Name: "image.png", MediaType: "image/png", Bytes: 1, Path: "/workspace/image.png"}},
	}}
	_, err = session.Preflight(context.Background(), request)
	assertProviderError(t, err, provider.ErrorImageInputUnsupported)
}

func TestStaleTurnSettingsRejectWithoutTransitionAndRefreshCatalog(t *testing.T) {
	runtime, requests, child := pipeRuntime(t)
	catalog := testNativeCatalog(t)
	runtime.catalog = catalog
	go runtime.readLoop(child.Output())
	initial, presentation, capabilities, err := catalog.resolveEffective("gpt-5.6-sol", "high", "priority")
	require.NoError(t, err)
	driver := &Driver{config: Config{Clock: fixedClock{time.Unix(100, 0).UTC()}, IDs: &sequenceIDs{}, IdleTimeout: time.Hour}, runtime: runtime}
	session := &Session{
		driver: driver, runtime: runtime, native: testCodexNativeSession(t, "native-thread", initial, presentation), threadID: "native-thread", capabilities: capabilities, catalog: catalog,
		events: make(chan provider.Event, 8), view: newSessionChild(), activities: make(map[string]string), toolStates: make(map[string]provider.ToolActivity), interactions: make(map[string]nativeInteraction),
	}
	runtime.sessions[session.threadID] = session
	result := make(chan error, 1)
	go func() {
		_, submitErr := session.Submit(context.Background(), provider.TurnRequest{TurnID: testID(945), MessageID: testID(946), Content: provider.TextMessage("stale"), Settings: &initial})
		result <- submitErr
	}()
	turnRequest := <-requests
	child.send(t, map[string]any{"id": turnRequest["id"], "error": map[string]any{"code": -32602, "message": "unsupported model setting"}})
	catalogRequest := <-requests
	var method string
	require.NoError(t, json.Unmarshal(catalogRequest["method"], &method))
	require.Equal(t, "model/list", method)
	var catalogResult any
	require.NoError(t, json.Unmarshal([]byte(testModelListJSON()), &catalogResult))
	child.send(t, map[string]any{"id": catalogRequest["id"], "result": catalogResult})
	assertProviderError(t, <-result, provider.ErrorInvalidModelConfiguration)
	require.Equal(t, initial, *session.NativeSession().Settings)
	session.mu.Lock()
	require.Nil(t, session.active)
	session.mu.Unlock()
	child.stop()
}

func TestSettingsNotificationWithoutThreadIdentityStopsRuntime(t *testing.T) {
	runtime := &runtime{input: discardWriteCloser{}, pending: make(map[int64]chan rpcResult), barriers: make(map[int64]*responseBarrier), inbound: make(map[string]struct{}), sessions: make(map[string]*Session), done: make(chan struct{})}
	runtime.handleNotification("thread/settings/updated", json.RawMessage(`{"threadSettings":{"model":"gpt-5.6-sol","effort":"high","serviceTier":null}}`))
	select {
	case <-runtime.done:
	default:
		t.Fatal("malformed settings notification did not stop the runtime")
	}
	assertProviderError(t, runtime.failure(), provider.ErrorMalformedStream)
}

func TestSettingsNotificationsAndReroutesNeverSynthesizePartialTuples(t *testing.T) {
	catalog := testNativeCatalog(t)
	initial, initialPresentation, initialCapabilities, err := catalog.resolveEffective("gpt-5.6-sol", "high", "priority")
	require.NoError(t, err)
	native := testCodexNativeSession(t, "native-thread", initial, initialPresentation)
	session := &Session{
		driver: &Driver{config: Config{Clock: fixedClock{time.Unix(100, 0).UTC()}, IDs: &sequenceIDs{}}}, native: native, threadID: "native-thread", capabilities: initialCapabilities,
		events: make(chan provider.Event, 16), view: newSessionChild(), activities: make(map[string]string), toolStates: make(map[string]provider.ToolActivity), interactions: make(map[string]nativeInteraction),
		active: &nativeTurn{request: provider.TurnRequest{TurnID: testID(950)}, nativeID: "native-turn"}, catalog: catalog,
	}

	session.handleNotification("thread/settings/updated", mustJSON(t, map[string]any{"threadId": "native-thread", "threadSettings": map[string]any{"model": "gpt-5.6-luna", "effort": "medium", "serviceTier": nil}}))
	verified := awaitEvent(t, session.events)
	require.Equal(t, provider.EventSettings, verified.Kind)
	require.Equal(t, provider.SettingsVerified, verified.SettingsState)
	require.Equal(t, "gpt-5.6-luna", verified.Settings.Model)

	session.handleNotification("model/rerouted", mustJSON(t, map[string]any{"threadId": "native-thread", "turnId": "native-turn", "fromModel": "gpt-5.6-luna", "toModel": "gpt-5.6-sol", "reason": "highRiskCyberActivity"}))
	unverified := awaitEvent(t, session.events)
	require.Equal(t, provider.EventSettings, unverified.Kind)
	require.Equal(t, provider.SettingsUnverified, unverified.SettingsState)
	require.Nil(t, unverified.Settings)

	session.handleNotification("thread/settings/updated", mustJSON(t, map[string]any{"threadId": "native-thread", "threadSettings": map[string]any{"model": "gpt-5.6-luna", "effort": "medium", "serviceTier": nil}}))
	select {
	case event := <-session.events:
		t.Fatalf("mismatched complete settings resolved a reroute: %#v", event)
	default:
	}
	session.handleNotification("thread/settings/updated", mustJSON(t, map[string]any{"threadId": "native-thread", "threadSettings": map[string]any{"model": "gpt-5.6-sol", "effort": "high", "serviceTier": "priority"}}))
	resolved := awaitEvent(t, session.events)
	require.Equal(t, provider.SettingsVerified, resolved.SettingsState)
	require.Equal(t, initial, *resolved.Settings)
}

func TestRerouteRejectsMatchingModelWithIncompatibleEffortOrFast(t *testing.T) {
	for name, threadSettings := range map[string]map[string]any{
		"effort": {"model": "gpt-5.6-luna", "effort": "high", "serviceTier": nil},
		"fast":   {"model": "gpt-5.6-luna", "effort": "medium", "serviceTier": "priority"},
	} {
		t.Run(name, func(t *testing.T) {
			catalog := testNativeCatalog(t)
			initial, presentation, capabilities, err := catalog.resolveEffective("gpt-5.6-sol", "high", "priority")
			require.NoError(t, err)
			session := &Session{
				driver: &Driver{config: Config{Clock: fixedClock{time.Unix(100, 0).UTC()}, IDs: &sequenceIDs{}}}, native: testCodexNativeSession(t, "native-thread", initial, presentation), threadID: "native-thread", capabilities: capabilities,
				events: make(chan provider.Event, 8), view: newSessionChild(), activities: make(map[string]string), toolStates: make(map[string]provider.ToolActivity), interactions: make(map[string]nativeInteraction),
				active: &nativeTurn{request: provider.TurnRequest{TurnID: testID(955)}, nativeID: "native-turn"}, catalog: catalog,
			}
			session.handleNotification("model/rerouted", mustJSON(t, map[string]any{"threadId": "native-thread", "turnId": "native-turn", "fromModel": "gpt-5.6-sol", "toModel": "gpt-5.6-luna", "reason": "highRiskCyberActivity"}))
			require.Equal(t, provider.SettingsUnverified, awaitEvent(t, session.events).SettingsState)
			session.handleNotification("thread/settings/updated", mustJSON(t, map[string]any{"threadId": "native-thread", "threadSettings": threadSettings}))
			failure := awaitEvent(t, session.events)
			require.Equal(t, provider.EventTerminalFailure, failure.Kind)
			require.Equal(t, provider.ErrorProtocolIncompatible, failure.Failure.Code())
		})
	}
}

func TestUnresolvedRerouteFailsTurnClosedAtTerminalBoundary(t *testing.T) {
	catalog := testNativeCatalog(t)
	initial, presentation, capabilities, err := catalog.resolveEffective("gpt-5.6-sol", "high", "priority")
	require.NoError(t, err)
	session := &Session{
		driver: &Driver{config: Config{Clock: fixedClock{time.Unix(100, 0).UTC()}, IDs: &sequenceIDs{}}}, native: testCodexNativeSession(t, "native-thread", initial, presentation), threadID: "native-thread", capabilities: capabilities,
		events: make(chan provider.Event, 8), view: newSessionChild(), activities: make(map[string]string), toolStates: make(map[string]provider.ToolActivity), interactions: make(map[string]nativeInteraction),
		active: &nativeTurn{request: provider.TurnRequest{TurnID: testID(960)}, nativeID: "native-turn"}, catalog: catalog,
	}
	session.handleNotification("model/rerouted", mustJSON(t, map[string]any{"threadId": "native-thread", "turnId": "native-turn", "fromModel": "gpt-5.6-sol", "toModel": "gpt-5.6-luna", "reason": "highRiskCyberActivity"}))
	require.Equal(t, provider.SettingsUnverified, awaitEvent(t, session.events).SettingsState)
	session.handleNotification("turn/completed", mustJSON(t, map[string]any{"threadId": "native-thread", "turn": map[string]any{"id": "native-turn", "status": "completed"}}))
	terminal := awaitEvent(t, session.events)
	require.Equal(t, provider.EventTerminalFailure, terminal.Kind)
	require.Equal(t, provider.ErrorProtocolIncompatible, terminal.Failure.Code())

	requestSettings := initial
	_, err = session.Submit(context.Background(), provider.TurnRequest{TurnID: testID(961), MessageID: testID(962), Content: provider.TextMessage("blocked"), Settings: &requestSettings})
	assertProviderError(t, err, provider.ErrorProtocolIncompatible)
}

func testNativeCatalog(t *testing.T) nativeCatalog {
	t.Helper()
	page, _, err := parseModelCatalogPage([]byte(testModelListJSON()))
	require.NoError(t, err)
	catalog, err := newNativeCatalog(page)
	require.NoError(t, err)
	return catalog
}

func testModelListJSON() string {
	return `{"data":[{"id":"fixture-alias","model":"gpt-fixture","displayName":"Fixture","description":"Default fixture","hidden":false,"isDefault":false,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium","description":"Balanced"}],"inputModalities":["text","image"],"serviceTiers":[]},{"id":"sol-alias","model":"gpt-5.6-sol","displayName":"5.6 Sol","description":"Deep coding","hidden":false,"isDefault":true,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium","description":"Balanced"},{"reasoningEffort":"high","description":"Deep"}],"inputModalities":["text","image"],"serviceTiers":[{"id":"priority","name":"Fast","description":"Priority"}]},{"id":"luna-alias","model":"gpt-5.6-luna","displayName":"5.6 Luna","description":"Execution","hidden":false,"isDefault":false,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium","description":"Balanced"}],"inputModalities":["text"],"serviceTiers":[]}],"nextCursor":null}`
}

func completeThreadResponse(threadID, model, effort string, tier any) map[string]any {
	return map[string]any{"thread": map[string]any{"id": threadID}, "model": model, "reasoningEffort": effort, "serviceTier": tier}
}

func testCodexNativeSession(t *testing.T, threadID string, settings provider.ExecutionSettings, presentation provider.ModelPresentation) provider.NativeSession {
	t.Helper()
	ref, err := provider.NewNativeSessionRef(threadID)
	require.NoError(t, err)
	now := time.Unix(100, 0).UTC()
	return provider.NativeSession{Ref: ref, Provider: provider.NameCodex, Model: settings.Model, Settings: &settings, Presentation: &presentation, CreatedAt: now, UpdatedAt: now}
}
