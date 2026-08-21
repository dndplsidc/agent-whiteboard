//go:build unix

package pi

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	"github.com/stretchr/testify/require"
)

func TestParseAvailableModelsRedactsAndAppliesThinkingMapSemantics(t *testing.T) {
	current := startupState{Model: "acme/reasoner", ThinkingLevel: "high"}
	raw := json.RawMessage(`{"models":[
		{"provider":"acme","id":"reasoner","name":"Reasoner","reasoning":true,"thinkingLevelMap":{"minimal":null,"xhigh":"xhigh","max":null},"input":["text","image"],"contextWindow":131072,"maxTokens":32768,"baseUrl":"https://secret.invalid","headers":{"Authorization":"secret"}},
		{"provider":"acme","id":"plain","name":"Plain","reasoning":false,"input":["text"],"contextWindow":32768,"maxTokens":4096,"cost":{"input":999}}
	]}`)
	catalog, private, err := parseAvailableModels(raw, current)
	require.NoError(t, err)
	require.NoError(t, catalog.Validate())
	require.Equal(t, []string{"off", "low", "medium", "high", "xhigh"}, effortValues(catalog.Models[0].SupportedReasoningEfforts))
	require.True(t, catalog.Models[0].SupportsImages)
	require.True(t, catalog.Models[0].Default)
	require.Equal(t, []string{"off"}, effortValues(catalog.Models[1].SupportedReasoningEfforts))
	require.Len(t, private, 2)
	encoded, err := json.Marshal(catalog)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "secret")
	require.NotContains(t, string(encoded), "baseUrl")
}

func TestPiPreflightResolvesTargetCapacityAndImageCapabilityWithoutMutation(t *testing.T) {
	session, child := newBehaviorSession(t)
	settings := provider.ExecutionSettings{Model: "model-provider/vision", Effort: "off", Speed: provider.SpeedStandard}
	request := provider.PreflightRequest{Turn: behaviorTurn(4, strings.Repeat("x", 100))}
	request.Turn.Settings = &settings
	request.Turn.Images = []provider.ImageInput{{ID: behaviorID(100), Name: "image.png", MediaType: "image/png", Bytes: 1, Path: "/private/missing.png"}}
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
	catalog := child.readCommand(t)
	require.Equal(t, "get_available_models", catalog["type"])
	child.writeRecord(t, responseRecord(catalog, map[string]any{"models": []any{
		map[string]any{"provider": "model-provider", "id": "model-id", "name": "Current", "reasoning": false, "input": []string{"text"}, "contextWindow": 32768, "maxTokens": 1024},
		map[string]any{"provider": "model-provider", "id": "vision", "name": "Vision", "reasoning": false, "input": []string{"text", "image"}, "contextWindow": 131072, "maxTokens": 32768},
	}}))
	respondPreflightState(t, child, 0, 32768, 1024)
	stats := child.readCommand(t)
	require.Equal(t, "get_session_stats", stats["type"])
	child.writeRecord(t, responseRecord(stats, map[string]any{"contextUsage": map[string]any{"tokens": 0, "contextWindow": 32768}}))
	resolved := <-answer
	require.NoError(t, resolved.err)
	require.Equal(t, settings.Model, resolved.result.ResolvedModel)
	require.Equal(t, 98304, resolved.result.EffectiveCapacityTokens)
	require.Equal(t, "model-provider/model-id", session.Model())

	session.mu.Lock()
	session.state.SupportsImages = true
	session.mu.Unlock()
	textSettings := provider.ExecutionSettings{Model: "model-provider/text", Effort: "off", Speed: provider.SpeedStandard}
	unsupported := provider.PreflightRequest{Turn: behaviorTurn(5, "reader")}
	unsupported.Turn.Settings = &textSettings
	unsupported.Turn.Images = request.Turn.Images
	failed := make(chan error, 1)
	go func() { _, err := session.Preflight(context.Background(), unsupported); failed <- err }()
	catalog = child.readCommand(t)
	require.Equal(t, "get_available_models", catalog["type"])
	child.writeRecord(t, responseRecord(catalog, map[string]any{"models": []any{
		map[string]any{"provider": "model-provider", "id": "model-id", "name": "Current", "reasoning": false, "input": []string{"text", "image"}, "contextWindow": 32768, "maxTokens": 1024},
		map[string]any{"provider": "model-provider", "id": "text", "name": "Text", "reasoning": false, "input": []string{"text"}, "contextWindow": 65536, "maxTokens": 8192},
	}}))
	assertProviderCode(t, <-failed, provider.ErrorImageInputUnsupported)
}

func TestParseAvailableModelsFailsClosedOnAmbiguityAndUnsupportedThinking(t *testing.T) {
	current := startupState{Model: "a/m", ThinkingLevel: "high"}
	for _, raw := range []string{
		`{"models":[{"provider":"a","id":"m","name":"M","reasoning":true,"thinkingLevelMap":{"turbo":"turbo"},"input":["text"]}]}`,
		`{"models":[{"provider":"a","id":"m","name":"M","reasoning":true,"thinkingLevelMap":{"high":null},"input":["text"]}]}`,
		`{"models":[{"provider":"a","id":"m","name":"M","reasoning":false,"input":["text"]},{"provider":"a","id":"m","name":"M2","reasoning":false,"input":["text"]}]}`,
	} {
		_, _, err := parseAvailableModels(json.RawMessage(raw), current)
		require.Error(t, err)
	}
}

func TestParsePiSkillsUsesCanonicalProvenanceAndStablePrivateBindings(t *testing.T) {
	raw := json.RawMessage(`{"commands":[
		{"name":"help","description":"builtin","source":"extension","sourceInfo":{"path":"/private/ext.ts"}},
		{"name":"skill:review","description":"Review safely","source":"skill","sourceInfo":{"path":"/private/skills/review/SKILL.md","source":"local","scope":"user","origin":"top-level","baseDir":"/private/skills/review"}}
	]}`)
	first, safe, err := parsePiSkills(raw)
	require.NoError(t, err)
	require.NoError(t, safe.Validate())
	second, safeAgain, err := parsePiSkills(raw)
	require.NoError(t, err)
	require.Equal(t, 1, safe.MaxSelectedSkills)
	require.Len(t, safe.Skills, 1)
	require.Equal(t, "review", safe.Skills[0].Name)
	require.Equal(t, provider.SkillScopeUser, safe.Skills[0].Scope)
	require.Equal(t, safe.Skills[0].ID, safeAgain.Skills[0].ID)
	require.Equal(t, "review", first.byID[safe.Skills[0].ID].name)
	require.Equal(t, first.byID, second.byID)
	encoded, _ := json.Marshal(safe)
	require.NotContains(t, string(encoded), "/private")
}

func TestParsePiSkillsEnforcesPinnedNameGrammarAndCLIProvenance(t *testing.T) {
	valid := json.RawMessage(`{"commands":[
		{"name":"skill:temporary-review","description":"Temporary CLI","source":"skill","sourceInfo":{"path":"/private/cli/SKILL.md","source":"cli","scope":"temporary","origin":"top-level"}},
		{"name":"skill:temporary-local","description":"Temporary local path","source":"skill","sourceInfo":{"path":"/private/path/SKILL.md","source":"local","scope":"temporary","origin":"top-level","baseDir":"/private/path"}},
		{"name":"skill:user-review","description":"User","source":"skill","sourceInfo":{"path":"/private/user/SKILL.md","source":"local","scope":"user","origin":"top-level","baseDir":"/private/user"}}
	]}`)
	_, safe, err := parsePiSkills(valid)
	require.NoError(t, err)
	require.Len(t, safe.Skills, 3)
	require.Equal(t, provider.SkillScopeSystem, safe.Skills[0].Scope)
	require.Equal(t, provider.SkillScopeSystem, safe.Skills[1].Scope)
	require.Equal(t, provider.SkillScopeUser, safe.Skills[2].Scope)
	encoded, err := json.Marshal(safe)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "/private")

	invalidNames := []string{"", "has space", `quote\"xml<`, "has/slash", "Upper", "-leading", "trailing-", "two--hyphens", strings.Repeat("a", 65)}
	for _, name := range invalidNames {
		raw := json.RawMessage(`{"commands":[{"name":"skill:` + name + `","description":"bad","source":"skill","sourceInfo":{"path":"/private/SKILL.md","source":"cli","scope":"temporary","origin":"top-level"}}]}`)
		_, _, err := parsePiSkills(raw)
		require.Error(t, err, name)
	}
	for _, sourceInfo := range []string{
		`{"path":"/private/SKILL.md","source":"cli","scope":"temporary","origin":"top-level","baseDir":"relative"}`,
		`{"path":"relative/SKILL.md","source":"cli","scope":"temporary","origin":"top-level"}`,
		`{"path":"/private/SKILL.md","source":"local","scope":"user","origin":"top-level"}`,
		`{"path":"/private/SKILL.md","source":"local","scope":"temporary","origin":"top-level"}`,
		`{"path":"/private/SKILL.md","source":"cli","scope":"user","origin":"top-level"}`,
		`{"path":"/private/SKILL.md","source":"local","scope":"path","origin":"top-level","baseDir":"/private"}`,
	} {
		raw := json.RawMessage(`{"commands":[{"name":"skill:review","description":"bad","source":"skill","sourceInfo":` + sourceInfo + `}]}`)
		_, _, err := parsePiSkills(raw)
		require.Error(t, err)
	}
}

func TestPiSkillExpansionRestoresOnlyCanonicalTerminalLF(t *testing.T) {
	turn := behaviorTurn(75, "reader")
	envelope, err := BuildEnvelope(turn)
	require.NoError(t, err)
	require.Equal(t, byte('\n'), envelope[len(envelope)-1])
	skill := piSkill{name: "review"}
	prefix := `<skill name="review" location="/private/SKILL.md">` + "\nReferences are relative to /private.\n\nbody\n</skill>\n\n"
	expanded := prefix + string(envelope[:len(envelope)-1])
	require.True(t, validatePiSkillExpansion(expanded, skill, envelope))
	require.False(t, validatePiSkillExpansion(expanded+"\n", skill, envelope))
	require.False(t, validatePiSkillExpansion(piSkillPrompt(skill, envelope), skill, envelope))
}

func TestPiSkillImageConstructionFailureDoesNotPanicOrWritePrompt(t *testing.T) {
	session, child := newBehaviorSession(t)
	session.mu.Lock()
	session.state.SupportsImages = true
	session.mu.Unlock()
	rawCommands := json.RawMessage(`{"commands":[{"name":"skill:review","description":"Review","source":"skill","sourceInfo":{"path":"/private/review/SKILL.md","source":"local","scope":"user","origin":"top-level","baseDir":"/private/review"}}]}`)
	_, safe, err := parsePiSkills(rawCommands)
	require.NoError(t, err)
	request := behaviorTurn(80, "reader")
	request.Content = provider.MessageContent{Parts: []provider.MessagePart{{Kind: provider.MessagePartSkill, Skill: &provider.SkillInvocation{ID: safe.Skills[0].ID, Name: "review"}}, {Kind: provider.MessagePartText, Text: "reader"}}}
	request.Images = []provider.ImageInput{{ID: behaviorID(120), Name: "missing.png", MediaType: "image/png", Bytes: 1, Path: "/missing/image.png"}}
	go func() {
		command := child.readCommand(t)
		require.Equal(t, "get_commands", command["type"])
		var commandData any
		require.NoError(t, json.Unmarshal(rawCommands, &commandData))
		child.writeRecord(t, responseRecord(command, commandData))
	}()
	var submitErr error
	require.NotPanics(t, func() { _, submitErr = session.Submit(context.Background(), request) })
	assertProviderCode(t, submitErr, provider.ErrorImageStorageFailure)
	require.Empty(t, session.rpc.writes)
}

func TestPiSkillReadFailureFailsClosedAndAbortsBeforeAcceptance(t *testing.T) {
	session, child := newBehaviorSession(t)
	rawCommands := json.RawMessage(`{"commands":[{"name":"skill:review","description":"Review","source":"skill","sourceInfo":{"path":"/private/review/SKILL.md","source":"local","scope":"user","origin":"top-level","baseDir":"/private/review"}}]}`)
	_, safe, err := parsePiSkills(rawCommands)
	require.NoError(t, err)
	request := behaviorTurn(81, "reader")
	request.Content = provider.MessageContent{Parts: []provider.MessagePart{{Kind: provider.MessagePartSkill, Skill: &provider.SkillInvocation{ID: safe.Skills[0].ID, Name: "review"}}, {Kind: provider.MessagePartText, Text: "reader"}}}
	answer := make(chan error, 1)
	go func() { _, submitErr := session.Submit(context.Background(), request); answer <- submitErr }()
	commands := child.readCommand(t)
	require.Equal(t, "get_commands", commands["type"])
	var commandData any
	require.NoError(t, json.Unmarshal(rawCommands, &commandData))
	child.writeRecord(t, responseRecord(commands, commandData))
	prompt := child.readCommand(t)
	require.Equal(t, "prompt", prompt["type"])
	require.Contains(t, prompt["message"], "/skill:review ")
	child.writeRecord(t, responseRecord(prompt, nil))
	child.writeRecord(t, nativeMessageRecord("message_start", "user", prompt["message"], "stop"))
	terminal := receiveProviderEvents(t, session.Events(), 1)[0]
	require.Equal(t, provider.EventTerminalFailure, terminal.Kind)
	require.Equal(t, provider.ErrorMalformedStream, terminal.Failure.Code())
	abort := child.readCommand(t)
	require.Equal(t, "abort", abort["type"])
	child.writeRecord(t, responseRecord(abort, nil))
	assertProviderCode(t, <-answer, provider.ErrorAcceptanceUnknown)
}

func TestPiSubmitValidatesUnreadableImageBeforeSettingsMutation(t *testing.T) {
	session, child := newBehaviorSession(t)
	request := behaviorTurn(84, "reader")
	settings := provider.ExecutionSettings{Model: "model-provider/next", Effort: "high", Speed: provider.SpeedStandard}
	request.Settings = &settings
	request.Images = []provider.ImageInput{{ID: behaviorID(124), Name: "missing.png", MediaType: "image/png", Bytes: 1, Path: "/missing/image.png"}}
	answer := make(chan error, 1)
	go func() { _, err := session.Submit(context.Background(), request); answer <- err }()
	respondPiSettingsCatalog(t, child)
	assertProviderCode(t, <-answer, provider.ErrorImageStorageFailure)
	require.Equal(t, "model-provider/model-id", session.Model())
}

func TestPiSubmitAdmissionKeepsSettingsAndPromptAtomicAgainstApply(t *testing.T) {
	session, child, _ := newPersistentBehaviorSession(t)
	settings := provider.ExecutionSettings{Model: "model-provider/next", Effort: "high", Speed: provider.SpeedStandard}
	request := behaviorTurn(85, "reader")
	request.Settings = &settings
	submit := make(chan error, 1)
	go func() { _, err := session.Submit(context.Background(), request); submit <- err }()
	respondPiSettingsCatalog(t, child)
	setModel := child.readCommand(t)
	require.Equal(t, "set_model", setModel["type"])
	child.writeRecord(t, responseRecord(setModel, nil))
	setEffort := child.readCommand(t)
	require.Equal(t, "set_thinking_level", setEffort["type"])
	child.writeRecord(t, responseRecord(setEffort, nil))
	respondPiEffectiveState(t, child, session, "next", "Next", "high")
	apply := make(chan error, 1)
	go func() { _, _, err := session.ApplySettings(context.Background(), settings); apply <- err }()
	prompt := child.readCommand(t)
	require.Equal(t, "prompt", prompt["type"])
	child.writeRecord(t, responseRecord(prompt, nil))
	require.NoError(t, <-submit)
	assertProviderCode(t, <-apply, provider.ErrorProtocolFailure)
}

func TestPiSubmitSettingsNotificationsDoNotMakePromptRejectionAmbiguous(t *testing.T) {
	session, child, _ := newPersistentBehaviorSession(t)
	settings := provider.ExecutionSettings{Model: "model-provider/next", Effort: "high", Speed: provider.SpeedStandard}
	request := behaviorTurn(86, "reader")
	request.Settings = &settings
	answer := make(chan error, 1)
	go func() { _, err := session.Submit(context.Background(), request); answer <- err }()
	respondPiSettingsCatalog(t, child)
	setModel := child.readCommand(t)
	require.Equal(t, "set_model", setModel["type"])
	child.writeRecord(t, map[string]any{"type": "session_info_changed"})
	child.writeRecord(t, responseRecord(setModel, nil))
	setEffort := child.readCommand(t)
	require.Equal(t, "set_thinking_level", setEffort["type"])
	child.writeRecord(t, map[string]any{"type": "thinking_level_changed"})
	child.writeRecord(t, responseRecord(setEffort, nil))
	respondPiEffectiveState(t, child, session, "next", "Next", "high")
	prompt := child.readCommand(t)
	require.Equal(t, "prompt", prompt["type"])
	child.writeRecord(t, map[string]any{"id": prompt["id"], "type": "response", "command": "prompt", "success": false, "error": "definite rejection"})
	assertProviderCode(t, <-answer, provider.ErrorProtocolFailure)
	session.mu.Lock()
	require.Nil(t, session.active)
	session.mu.Unlock()
	event := receiveProviderEvents(t, session.Events(), 1)[0]
	require.Equal(t, provider.EventSettings, event.Kind)
	require.Equal(t, settings, *event.Settings)
	select {
	case duplicate := <-session.Events():
		t.Fatalf("unexpected duplicate settings/turn event: %#v", duplicate)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestPiDelayedSettingsNotificationDoesNotMakePromptRejectionAmbiguous(t *testing.T) {
	for _, eventType := range []string{"thinking_level_changed", "session_info_changed"} {
		t.Run(eventType, func(t *testing.T) {
			session, child := newBehaviorSession(t)
			answer := make(chan error, 1)
			go func() { _, err := session.Submit(context.Background(), behaviorTurn(98, "reader")); answer <- err }()
			prompt := child.readCommand(t)
			require.Equal(t, "prompt", prompt["type"])
			child.writeRecord(t, map[string]any{"type": eventType})
			child.writeRecord(t, map[string]any{"id": prompt["id"], "type": "response", "command": "prompt", "success": false, "error": "definite rejection"})
			assertProviderCode(t, <-answer, provider.ErrorProtocolFailure)
			session.mu.Lock()
			require.Nil(t, session.active)
			session.mu.Unlock()
			select {
			case event := <-session.Events():
				t.Fatalf("settings-only notification emitted a provider event: %#v", event)
			case <-time.After(20 * time.Millisecond):
			}
		})
	}
}

func TestPiSubmitCancellationAfterSettingsWriteReconcilesAndPersistsActual(t *testing.T) {
	for _, test := range []struct {
		name      string
		requested provider.ExecutionSettings
		command   string
		modelID   string
		modelName string
		effort    string
	}{
		{name: "after model", requested: provider.ExecutionSettings{Model: "model-provider/next", Effort: "off", Speed: provider.SpeedStandard}, command: "set_model", modelID: "next", modelName: "Next", effort: "off"},
		{name: "during effort", requested: provider.ExecutionSettings{Model: "model-provider/model-id", Effort: "high", Speed: provider.SpeedStandard}, command: "set_thinking_level", modelID: "model-id", modelName: "Current", effort: "high"},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, child, manager := newPersistentBehaviorSession(t)
			request := behaviorTurn(94, "reader")
			request.Settings = &test.requested
			ctx, cancel := context.WithCancel(context.Background())
			answer := make(chan error, 1)
			go func() { _, err := session.Submit(ctx, request); answer <- err }()
			respondPiSettingsCatalog(t, child)
			settingsCommand := child.readCommand(t)
			require.Equal(t, test.command, settingsCommand["type"])
			if test.command == "set_model" {
				child.writeRecord(t, responseRecord(settingsCommand, nil))
				recovery := child.readCommand(t)
				require.Equal(t, "get_state", recovery["type"])
				cancel()
				respondPiStateCommand(t, child, session, recovery, test.modelID, test.modelName, test.effort)
			} else {
				cancel()
				recovery := child.readCommand(t)
				require.Equal(t, "get_state", recovery["type"])
				respondPiStateCommand(t, child, session, recovery, test.modelID, test.modelName, test.effort)
			}
			require.ErrorIs(t, <-answer, context.Canceled)
			inspected, err := manager.inspect(session.NativeSession().Ref)
			require.NoError(t, err)
			require.Equal(t, test.requested, *inspected.Settings)
			require.Equal(t, test.requested, *session.NativeSession().Settings)
			event := receiveProviderEvents(t, session.Events(), 1)[0]
			require.Equal(t, provider.EventSettings, event.Kind)
			require.Equal(t, test.requested, *event.Settings)
			session.mu.Lock()
			require.Nil(t, session.active)
			session.mu.Unlock()
		})
	}
}

func TestPiSubmitSettingsReconciliationFailsClosedOnNativeBusyState(t *testing.T) {
	for _, test := range []struct {
		name         string
		streaming    bool
		compacting   bool
		pendingCount int
	}{
		{name: "streaming", streaming: true},
		{name: "compacting", compacting: true},
		{name: "pending", pendingCount: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, child, manager := newPersistentBehaviorSession(t)
			before := session.NativeSession()
			requested := provider.ExecutionSettings{Model: "model-provider/next", Effort: "high", Speed: provider.SpeedStandard}
			request := behaviorTurn(97, "reader")
			request.Settings = &requested
			answer := make(chan error, 1)
			go func() { _, err := session.Submit(context.Background(), request); answer <- err }()
			respondPiSettingsCatalog(t, child)
			setModel := child.readCommand(t)
			require.Equal(t, "set_model", setModel["type"])
			child.writeRecord(t, responseRecord(setModel, nil))
			setEffort := child.readCommand(t)
			require.Equal(t, "set_thinking_level", setEffort["type"])
			child.writeRecord(t, responseRecord(setEffort, nil))
			state := child.readCommand(t)
			require.Equal(t, "get_state", state["type"])
			respondPiBusyStateCommand(t, child, session, state, test.streaming, test.compacting, test.pendingCount)
			assertProviderCode(t, <-answer, provider.ErrorProtocolFailure)
			select {
			case <-session.rpc.done:
			default:
				t.Fatal("session remained open after authoritative busy-state reconciliation failure")
			}
			line, readErr := child.inputReader.ReadBytes('\n')
			require.Empty(t, line, "prompt was written after busy settings reconciliation")
			require.Error(t, readErr)
			inspected, err := manager.inspect(before.Ref)
			require.NoError(t, err)
			require.Equal(t, *before.Settings, *inspected.Settings)
			require.Equal(t, *before.Settings, *session.NativeSession().Settings)
			session.mu.Lock()
			require.Nil(t, session.active)
			session.mu.Unlock()
		})
	}
}

func TestPiApplySettingsPersistsSuccessfulAuthoritativeTransition(t *testing.T) {
	session, child, manager := newPersistentBehaviorSession(t)
	originalRef := session.NativeSession().Ref
	requested := provider.ExecutionSettings{Model: "model-provider/next", Effort: "high", Speed: provider.SpeedStandard}
	answer := make(chan error, 1)
	go func() { _, _, err := session.ApplySettings(context.Background(), requested); answer <- err }()
	respondPiSettingsCatalog(t, child)
	setModel := child.readCommand(t)
	require.Equal(t, "set_model", setModel["type"])
	child.writeRecord(t, responseRecord(setModel, map[string]any{}))
	setEffort := child.readCommand(t)
	require.Equal(t, "set_thinking_level", setEffort["type"])
	child.writeRecord(t, responseRecord(setEffort, nil))
	respondPiEffectiveState(t, child, session, "next", "Next", "high")
	require.NoError(t, <-answer)
	inspected, err := manager.inspect(originalRef)
	require.NoError(t, err)
	require.Equal(t, originalRef, inspected.Ref)
	require.Equal(t, requested, *inspected.Settings)
	require.Equal(t, "Next", inspected.Presentation.ModelDisplayName)
}

func TestPiApplySettingsReportsAndPersistsAuthoritativeStateAfterPartialFailure(t *testing.T) {
	session, child, manager := newPersistentBehaviorSession(t)
	originalRef := session.NativeSession().Ref
	requested := provider.ExecutionSettings{Model: "model-provider/next", Effort: "high", Speed: provider.SpeedStandard}
	answer := make(chan struct {
		settings provider.ExecutionSettings
		err      error
	}, 1)
	go func() {
		settings, _, err := session.ApplySettings(context.Background(), requested)
		answer <- struct {
			settings provider.ExecutionSettings
			err      error
		}{settings, err}
	}()
	respondPiSettingsCatalog(t, child)
	setModel := child.readCommand(t)
	require.Equal(t, "set_model", setModel["type"])
	child.writeRecord(t, responseRecord(setModel, map[string]any{}))
	setEffort := child.readCommand(t)
	require.Equal(t, "set_thinking_level", setEffort["type"])
	child.writeRecord(t, map[string]any{"id": setEffort["id"], "type": "response", "command": "set_thinking_level", "success": false, "error": "private native failure"})
	respondPiEffectiveState(t, child, session, "next", "Next", "off")
	resolved := <-answer
	assertProviderCode(t, resolved.err, provider.ErrorInvalidModelConfiguration)
	actual := provider.ExecutionSettings{Model: "model-provider/next", Effort: "off", Speed: provider.SpeedStandard}
	require.Equal(t, actual, resolved.settings)
	inspected, err := manager.inspect(originalRef)
	require.NoError(t, err)
	require.Equal(t, originalRef, inspected.Ref)
	require.Equal(t, actual, *inspected.Settings)
	event := receiveProviderEvents(t, session.Events(), 1)[0]
	require.Equal(t, provider.EventSettings, event.Kind)
	require.Equal(t, resolved.settings, *event.Settings)
}

func TestPiSettingsTimestampDoesNotMoveBackward(t *testing.T) {
	session, child, manager := newPersistentBehaviorSession(t)
	before := session.NativeSession()
	session.driver.config.Clock = fixedClock{value: before.UpdatedAt.Add(-time.Hour)}
	requested := provider.ExecutionSettings{Model: "model-provider/next", Effort: "high", Speed: provider.SpeedStandard}
	answer := make(chan error, 1)
	go func() { _, _, err := session.ApplySettings(context.Background(), requested); answer <- err }()
	respondPiSettingsCatalog(t, child)
	setModel := child.readCommand(t)
	child.writeRecord(t, responseRecord(setModel, nil))
	setEffort := child.readCommand(t)
	child.writeRecord(t, responseRecord(setEffort, nil))
	respondPiEffectiveState(t, child, session, "next", "Next", "high")
	require.NoError(t, <-answer)
	require.Equal(t, before.UpdatedAt, session.NativeSession().UpdatedAt)
	persisted, err := manager.inspect(before.Ref)
	require.NoError(t, err)
	require.Equal(t, before.UpdatedAt, persisted.UpdatedAt)
}

func TestPiCompactEndBeforeStartFailsClosed(t *testing.T) {
	session, child := newBehaviorSession(t)
	request := provider.CompactRequest{WorkID: behaviorID(74)}
	answer := make(chan error, 1)
	go func() { _, err := session.Compact(context.Background(), request); answer <- err }()
	command := child.readCommand(t)
	require.Equal(t, "compact", command["type"])
	child.writeRecord(t, map[string]any{"type": "compaction_end"})
	child.writeRecord(t, responseRecord(command, nil))
	assertProviderCode(t, <-answer, provider.ErrorProtocolFailure)
	event := receiveProviderEvents(t, session.Events(), 1)[0]
	require.Equal(t, provider.CompactFailed, event.Compact.Status)
	child.writeRecord(t, map[string]any{"type": "compaction_start"})
	select {
	case duplicate := <-session.Events():
		t.Fatalf("unexpected compact event after failed ordering: %#v", duplicate)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestPiCompactWriteThenCancelRetainsOwnershipUntilLateTerminal(t *testing.T) {
	session, child := newBehaviorSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	request := provider.CompactRequest{WorkID: behaviorID(99)}
	answer := make(chan error, 1)
	go func() { _, err := session.Compact(ctx, request); answer <- err }()
	command := child.readCommand(t)
	require.Equal(t, "compact", command["type"])
	cancel()
	assertProviderCode(t, <-answer, provider.ErrorAcceptanceUnknown)
	assertPiCompactAdmissionBlocked(t, session)
	child.writeRecord(t, map[string]any{"type": "compaction_start"})
	child.writeRecord(t, map[string]any{"type": "compaction_end"})
	event := receiveProviderEvents(t, session.Events(), 1)[0]
	require.Equal(t, provider.EventCompact, event.Kind)
	require.Equal(t, provider.CompactCompleted, event.Compact.Status)
	require.Equal(t, request.WorkID, event.Compact.WorkID)
	select {
	case duplicate := <-session.Events():
		t.Fatalf("duplicate compact terminal: %#v", duplicate)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestPiCompactStartBeforeNegativeResponseRetainsOwnershipUntilTerminal(t *testing.T) {
	session, child := newBehaviorSession(t)
	request := provider.CompactRequest{WorkID: behaviorID(100)}
	answer := make(chan error, 1)
	go func() { _, err := session.Compact(context.Background(), request); answer <- err }()
	command := child.readCommand(t)
	require.Equal(t, "compact", command["type"])
	child.writeRecord(t, map[string]any{"type": "compaction_start"})
	child.writeRecord(t, map[string]any{"id": command["id"], "type": "response", "command": "compact", "success": false, "error": "contradictory rejection"})
	assertProviderCode(t, <-answer, provider.ErrorAcceptanceUnknown)
	assertPiCompactAdmissionBlocked(t, session)
	child.writeRecord(t, map[string]any{"type": "compaction_end"})
	event := receiveProviderEvents(t, session.Events(), 1)[0]
	require.Equal(t, provider.EventCompact, event.Kind)
	require.Equal(t, provider.CompactCompleted, event.Compact.Status)
	require.Equal(t, request.WorkID, event.Compact.WorkID)
	select {
	case duplicate := <-session.Events():
		t.Fatalf("duplicate compact terminal: %#v", duplicate)
	case <-time.After(20 * time.Millisecond):
	}
}

func assertPiCompactAdmissionBlocked(t *testing.T, session *Session) {
	t.Helper()
	session.mu.Lock()
	require.NotNil(t, session.compact)
	session.mu.Unlock()
	_, err := session.Submit(context.Background(), behaviorTurn(101, "blocked"))
	assertProviderCode(t, err, provider.ErrorProtocolFailure)
	_, _, err = session.ApplySettings(context.Background(), provider.ExecutionSettings{Model: "model-provider/model-id", Effort: "off", Speed: provider.SpeedStandard})
	assertProviderCode(t, err, provider.ErrorProtocolFailure)
	_, err = session.Compact(context.Background(), provider.CompactRequest{WorkID: behaviorID(102)})
	assertProviderCode(t, err, provider.ErrorProtocolFailure)
}

func TestPiCompactCorrelatesStartResponseAndOneTerminal(t *testing.T) {
	session, child := newBehaviorSession(t)
	answer := make(chan struct {
		accepted provider.AcceptedCompact
		err      error
	}, 1)
	request := provider.CompactRequest{WorkID: behaviorID(76)}
	go func() {
		accepted, err := session.Compact(context.Background(), request)
		answer <- struct {
			accepted provider.AcceptedCompact
			err      error
		}{accepted, err}
	}()
	command := child.readCommand(t)
	require.Equal(t, "compact", command["type"])
	child.writeRecord(t, map[string]any{"type": "compaction_start"})
	child.writeRecord(t, responseRecord(command, nil))
	resolved := <-answer
	require.NoError(t, resolved.err)
	require.Equal(t, request.WorkID, resolved.accepted.WorkID)
	child.writeRecord(t, map[string]any{"type": "compaction_end"})
	event := receiveProviderEvents(t, session.Events(), 1)[0]
	require.Equal(t, provider.CompactCompleted, event.Compact.Status)
	child.writeRecord(t, map[string]any{"type": "compaction_end"})
	select {
	case duplicate := <-session.Events():
		t.Fatalf("duplicate compact terminal: %#v", duplicate)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestPiInteractionDeadlineAutonomouslyResolvesWithoutResponse(t *testing.T) {
	session, _, timers, request := newManualTimedInteraction(t, 103)
	timers.timer(t, 0).Fire()
	resolved := receiveProviderEvents(t, session.Events(), 1)[0]
	require.Equal(t, provider.EventInteractionResolved, resolved.Kind)
	require.Equal(t, request.ID, resolved.Resolution.RequestID)
	require.Equal(t, request.Kind, resolved.Resolution.Kind)
	session.mu.Lock()
	_, pending := session.interactions[request.ID]
	session.mu.Unlock()
	require.False(t, pending)
}

func TestPiInteractionTimerExpiryRacesShutdownAndChildExitWithoutLosingResolution(t *testing.T) {
	t.Run("shutdown", func(t *testing.T) {
		session, child, timers, request := newManualTimedInteraction(t, 114)
		timer := timers.timer(t, 0)
		start := make(chan struct{})
		fired := make(chan struct{})
		shutdown := make(chan error, 1)
		go func() { <-start; timer.Fire(); close(fired) }()
		go func() { <-start; shutdown <- session.Shutdown(context.Background()) }()
		close(start)
		line, readErr := child.inputReader.ReadBytes('\n')
		if readErr == nil {
			var wire map[string]any
			require.NoError(t, json.Unmarshal(line, &wire))
			require.Equal(t, "extension_ui_response", wire["type"])
			require.Equal(t, true, wire["cancelled"])
		} else {
			require.Empty(t, line)
		}
		<-fired
		events := finishPiShutdown(t, session, child, shutdown)
		require.Equal(t, 1, countPiInteractionResolutions(events, request.ID))
	})
	t.Run("child exit", func(t *testing.T) {
		session, child, timers, request := newManualTimedInteraction(t, 115)
		timer := timers.timer(t, 0)
		start := make(chan struct{})
		fired := make(chan struct{})
		exited := make(chan struct{})
		go func() { <-start; timer.Fire(); close(fired) }()
		go func() { <-start; child.closeOutput(); close(exited) }()
		close(start)
		<-fired
		<-exited
		select {
		case <-session.loopsDone:
		case <-time.After(time.Second):
			t.Fatal("session did not close after child exit")
		}
		events := make([]provider.Event, 0)
		for event := range session.Events() {
			events = append(events, event)
		}
		require.Equal(t, 1, countPiInteractionResolutions(events, request.ID))
	})
}

func TestPiExpiredRespondRacesChildExitWithoutLosingResolution(t *testing.T) {
	session, child, _, request := newManualTimedInteraction(t, 116)
	session.driver.config.Clock = fixedClock{value: request.LocalDeadline.Add(time.Millisecond)}
	response := provider.InteractionResponse{RequestID: request.ID, Kind: request.Kind, Answers: map[string][]string{"value": {"option-1"}}}
	start := make(chan struct{})
	responded := make(chan error, 1)
	exited := make(chan struct{})
	go func() { <-start; responded <- session.Respond(context.Background(), response) }()
	go func() { <-start; child.closeOutput(); close(exited) }()
	close(start)
	assertProviderCode(t, <-responded, provider.ErrorProtocolFailure)
	<-exited
	select {
	case <-session.loopsDone:
	case <-time.After(time.Second):
		t.Fatal("session did not close after child exit")
	}
	events := make([]provider.Event, 0)
	for event := range session.Events() {
		events = append(events, event)
	}
	require.Equal(t, 1, countPiInteractionResolutions(events, request.ID))
}

func TestPiInteractionResponseWinsBeforeDeadlineAndDisarmsTimer(t *testing.T) {
	session, child, timers, request := newManualTimedInteraction(t, 104)
	response := provider.InteractionResponse{RequestID: request.ID, Kind: request.Kind, Answers: map[string][]string{"value": {"option-1"}}}
	answer := make(chan error, 1)
	go func() { answer <- session.Respond(context.Background(), response) }()
	wire := child.readCommand(t)
	require.Equal(t, "extension_ui_response", wire["type"])
	require.NoError(t, <-answer)
	require.True(t, timers.timer(t, 0).Stopped())
	timers.timer(t, 0).Fire()
	assertNoPiInteractionEvent(t, session.Events())
}

func TestPiInteractionDeadlineWinsBeforeResponseAndRepeatedFireIsIdempotent(t *testing.T) {
	session, _, timers, request := newManualTimedInteraction(t, 105)
	timer := timers.timer(t, 0)
	timer.Fire()
	resolved := receiveProviderEvents(t, session.Events(), 1)[0]
	require.Equal(t, provider.EventInteractionResolved, resolved.Kind)
	response := provider.InteractionResponse{RequestID: request.ID, Kind: request.Kind, Answers: map[string][]string{"value": {"option-1"}}}
	assertProviderCode(t, session.Respond(context.Background(), response), provider.ErrorProtocolFailure)
	timer.Fire()
	assertNoPiInteractionEvent(t, session.Events())
}

func TestPiInteractionCancellationAndTurnSettlementDisarmDeadline(t *testing.T) {
	t.Run("broker cancellation", func(t *testing.T) {
		session, child, timers, request := newManualTimedInteraction(t, 106)
		answer := make(chan error, 1)
		go func() { answer <- session.CancelInteraction(context.Background(), request.ID) }()
		wire := child.readCommand(t)
		require.Equal(t, "extension_ui_response", wire["type"])
		require.Equal(t, true, wire["cancelled"])
		require.NoError(t, <-answer)
		require.True(t, timers.timer(t, 0).Stopped())
		timers.timer(t, 0).Fire()
		assertNoPiInteractionEvent(t, session.Events())
	})
	t.Run("turn settlement", func(t *testing.T) {
		session, _, timers, request := newManualTimedInteraction(t, 107)
		session.mu.Lock()
		turn := session.active
		session.mu.Unlock()
		session.finishTurn(turn, false)
		resolved := receiveProviderEvents(t, session.Events(), 1)[0]
		require.Equal(t, provider.EventInteractionResolved, resolved.Kind)
		require.Equal(t, request.ID, resolved.Resolution.RequestID)
		require.True(t, timers.timer(t, 0).Stopped())
		timers.timer(t, 0).Fire()
		assertNoPiInteractionEvent(t, session.Events())
	})
}

func TestPiShutdownClaimsPendingInteractionAndResolvesExactlyOnce(t *testing.T) {
	session, child, timers, request := newManualTimedInteraction(t, 108)
	shutdown := make(chan error, 1)
	go func() { shutdown <- session.Shutdown(context.Background()) }()
	wire := child.readCommand(t)
	require.Equal(t, "extension_ui_response", wire["type"])
	require.Equal(t, "native-timed", wire["id"])
	require.Equal(t, true, wire["cancelled"])
	events := finishPiShutdown(t, session, child, shutdown)
	require.Equal(t, 1, countPiInteractionResolutions(events, request.ID))
	require.True(t, timers.timer(t, 0).Stopped())
	timers.timer(t, 0).Fire()
}

func TestPiInteractionTimerAndShutdownPreserveFirstOwner(t *testing.T) {
	t.Run("timer wins", func(t *testing.T) {
		session, child, timers, request := newManualTimedInteraction(t, 109)
		timers.timer(t, 0).Fire()
		shutdown := make(chan error, 1)
		go func() { shutdown <- session.Shutdown(context.Background()) }()
		line, err := child.inputReader.ReadBytes('\n')
		require.Empty(t, line, "shutdown sent a second native response after timer ownership")
		require.Error(t, err)
		events := finishPiShutdown(t, session, child, shutdown)
		require.Equal(t, 1, countPiInteractionResolutions(events, request.ID))
	})
	t.Run("shutdown wins", func(t *testing.T) {
		session, child, timers, request := newManualTimedInteraction(t, 110)
		shutdown := make(chan error, 1)
		go func() { shutdown <- session.Shutdown(context.Background()) }()
		wire := child.readCommand(t)
		require.Equal(t, "extension_ui_response", wire["type"])
		timers.timer(t, 0).Fire()
		events := finishPiShutdown(t, session, child, shutdown)
		require.Equal(t, 1, countPiInteractionResolutions(events, request.ID))
		require.True(t, timers.timer(t, 0).Stopped())
	})
}

func TestPiInteractionRespondAndShutdownPreserveFirstOwner(t *testing.T) {
	t.Run("respond wins", func(t *testing.T) {
		session, child, _, request := newManualTimedInteraction(t, 111)
		response := provider.InteractionResponse{RequestID: request.ID, Kind: request.Kind, Answers: map[string][]string{"value": {"option-1"}}}
		responded := make(chan error, 1)
		go func() { responded <- session.Respond(context.Background(), response) }()
		wire := child.readCommand(t)
		require.Equal(t, "extension_ui_response", wire["type"])
		require.NoError(t, <-responded)
		shutdown := make(chan error, 1)
		go func() { shutdown <- session.Shutdown(context.Background()) }()
		line, err := child.inputReader.ReadBytes('\n')
		require.Empty(t, line, "shutdown sent a second native response after Respond ownership")
		require.Error(t, err)
		events := finishPiShutdown(t, session, child, shutdown)
		require.Equal(t, 0, countPiInteractionResolutions(events, request.ID))
	})
	t.Run("shutdown wins", func(t *testing.T) {
		session, child, _, request := newManualTimedInteraction(t, 112)
		shutdown := make(chan error, 1)
		go func() { shutdown <- session.Shutdown(context.Background()) }()
		wire := child.readCommand(t)
		require.Equal(t, "extension_ui_response", wire["type"])
		response := provider.InteractionResponse{RequestID: request.ID, Kind: request.Kind, Answers: map[string][]string{"value": {"option-1"}}}
		assertProviderCode(t, session.Respond(context.Background(), response), provider.ErrorProtocolFailure)
		events := finishPiShutdown(t, session, child, shutdown)
		require.Equal(t, 1, countPiInteractionResolutions(events, request.ID))
	})
}

func TestPiShutdownResolvesInteractionWhenNativeCancellationWriteFails(t *testing.T) {
	session, child, timers, request := newManualTimedInteraction(t, 113)
	require.NoError(t, session.rpc.input.Close())
	shutdown := make(chan error, 1)
	go func() { shutdown <- session.Shutdown(context.Background()) }()
	events := finishPiShutdown(t, session, child, shutdown)
	require.Equal(t, 1, countPiInteractionResolutions(events, request.ID))
	require.True(t, timers.timer(t, 0).Stopped())
}

func TestPiExtensionInteractionOwnsFirstCompleteResponseAndOmitsChrome(t *testing.T) {
	session, child := newBehaviorSession(t)
	session.driver.config.IDs = fixedIDs{value: behaviorID(77)}
	turn := installBehaviorTurn(session, 78)
	child.writeRecord(t, map[string]any{"type": "extension_ui_request", "id": "chrome", "method": "setTitle", "title": "secret"})
	child.writeRecord(t, map[string]any{"type": "extension_ui_request", "id": "native-select", "method": "select", "title": "Choose", "options": []string{"Allow", "Block"}, "timeout": 10000})
	receivedAt := session.driver.config.Clock.Now().UTC()
	event := receiveProviderEvents(t, session.Events(), 1)[0]
	require.Equal(t, provider.EventInteractionRequest, event.Kind)
	require.Equal(t, turn.request.TurnID, event.TurnID)
	require.NotNil(t, event.Interaction.LocalDeadline)
	require.True(t, event.Interaction.LocalDeadline.After(receivedAt))
	require.True(t, event.Interaction.LocalDeadline.Before(receivedAt.Add(10*time.Second)))
	response := provider.InteractionResponse{RequestID: event.Interaction.ID, Kind: event.Interaction.Kind, Answers: map[string][]string{"value": {"option-1"}}}
	responded := make(chan error, 1)
	go func() { responded <- session.Respond(context.Background(), response) }()
	wire := child.readCommand(t)
	require.Equal(t, "extension_ui_response", wire["type"])
	require.Equal(t, "native-select", wire["id"])
	require.Equal(t, "Allow", wire["value"])
	require.NoError(t, <-responded)
	assertProviderCode(t, session.Respond(context.Background(), response), provider.ErrorProtocolFailure)
	select {
	case duplicate := <-session.Events():
		t.Fatalf("broker-owned response emitted duplicate resolution: %#v", duplicate)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestPiExtensionRejectsUnboundedTimeout(t *testing.T) {
	session, child := newBehaviorSession(t)
	session.driver.config.IDs = fixedIDs{value: behaviorID(78)}
	installBehaviorTurn(session, 79)
	child.writeRecord(t, map[string]any{"type": "extension_ui_request", "id": "native-overflow", "method": "select", "title": "Choose", "options": []string{"Allow"}, "timeout": int64(^uint64(0) >> 1)})
	cancel := child.readCommand(t)
	require.Equal(t, "extension_ui_response", cancel["type"])
	require.Equal(t, "native-overflow", cancel["id"])
	require.Equal(t, true, cancel["cancelled"])
	terminal := receiveProviderEvents(t, session.Events(), 1)[0]
	require.Equal(t, provider.EventTerminalFailure, terminal.Kind)
	require.Equal(t, provider.ErrorMalformedStream, terminal.Failure.Code())
	abort := child.readCommand(t)
	require.Equal(t, "abort", abort["type"])
	child.writeRecord(t, responseRecord(abort, nil))
}

func TestPiMalformedBlockingInteractionsCancelExactlyOnceBeforeAbort(t *testing.T) {
	cases := []map[string]any{
		{"type": "extension_ui_request", "id": "bad-options", "method": "select", "title": "Choose", "options": []string{}},
		{"type": "extension_ui_request", "id": "bad-field", "method": "input", "title": "Input", "placeholder": strings.Repeat("x", provider.MaxInteractionTextBytes+1)},
		{"type": "extension_ui_request", "id": "bad-timeout", "method": "confirm", "title": "Confirm", "timeout": -1},
	}
	for _, event := range cases {
		t.Run(event["id"].(string), func(t *testing.T) {
			session, child := newBehaviorSession(t)
			session.driver.config.IDs = fixedIDs{value: behaviorID(91)}
			installBehaviorTurn(session, 89)
			child.writeRecord(t, event)
			cancel := child.readCommand(t)
			require.Equal(t, "extension_ui_response", cancel["type"])
			require.Equal(t, event["id"], cancel["id"])
			require.Equal(t, true, cancel["cancelled"])
			terminal := receiveProviderEvents(t, session.Events(), 1)[0]
			require.Equal(t, provider.EventTerminalFailure, terminal.Kind)
			abort := child.readCommand(t)
			require.Equal(t, "abort", abort["type"])
			child.writeRecord(t, responseRecord(abort, nil))
		})
	}
}

func TestPiDuplicateBlockingNativeIDCancelsBeforeAbort(t *testing.T) {
	session, child := newBehaviorSession(t)
	session.driver.config.IDs = fixedIDs{value: behaviorID(95)}
	installBehaviorTurn(session, 96)
	request := map[string]any{"type": "extension_ui_request", "id": "duplicate-native", "method": "select", "title": "Choose", "options": []string{"Allow"}}
	child.writeRecord(t, request)
	first := receiveProviderEvents(t, session.Events(), 1)[0]
	require.Equal(t, provider.EventInteractionRequest, first.Kind)
	child.writeRecord(t, request)
	cancel := child.readCommand(t)
	require.Equal(t, "extension_ui_response", cancel["type"])
	require.Equal(t, "duplicate-native", cancel["id"])
	require.Equal(t, true, cancel["cancelled"])
	settled := receiveProviderEvents(t, session.Events(), 2)
	require.Equal(t, provider.EventInteractionResolved, settled[0].Kind)
	require.Equal(t, first.Interaction.ID, settled[0].Resolution.RequestID)
	require.Equal(t, provider.EventTerminalFailure, settled[1].Kind)
	abort := child.readCommand(t)
	require.Equal(t, "abort", abort["type"])
	child.writeRecord(t, responseRecord(abort, nil))
}

func TestPiMCPInputEditorDeclineAndCancelOwnFirstResponse(t *testing.T) {
	for _, test := range []struct{ method, option string }{{"input", "decline"}, {"editor", "cancel"}} {
		t.Run(test.method+"_"+test.option, func(t *testing.T) {
			session, child := newBehaviorSession(t)
			session.driver.config.IDs = fixedIDs{value: behaviorID(82)}
			installBehaviorTurn(session, 83)
			child.writeRecord(t, map[string]any{"type": "extension_ui_request", "id": "native-" + test.method, "method": test.method, "title": "Provide value"})
			event := receiveProviderEvents(t, session.Events(), 1)[0]
			require.Equal(t, provider.EventInteractionRequest, event.Kind)
			response := provider.InteractionResponse{RequestID: event.Interaction.ID, Kind: event.Interaction.Kind, OptionID: test.option, Answers: map[string][]string{}}
			responded := make(chan error, 1)
			go func() { responded <- session.Respond(context.Background(), response) }()
			wire := child.readCommand(t)
			require.Equal(t, "extension_ui_response", wire["type"])
			require.Equal(t, true, wire["cancelled"])
			require.NotContains(t, wire, "value")
			require.NoError(t, <-responded)
			assertProviderCode(t, session.Respond(context.Background(), response), provider.ErrorProtocolFailure)
			select {
			case duplicate := <-session.Events():
				t.Fatalf("broker-owned cancellation emitted duplicate resolution: %#v", duplicate)
			case <-time.After(20 * time.Millisecond):
			}
		})
	}
}

func TestPiRespondDeadlineCoversBlockedNativeWrite(t *testing.T) {
	input := newBlockingInput()
	outputReader, outputWriter := io.Pipe()
	child := &specialRPCChild{input: input, output: outputReader}
	client, err := newRPCClient(child)
	require.NoError(t, err)
	now := time.Now().UTC()
	session := &Session{driver: &Driver{config: Config{Clock: fixedClock{value: now}}}, rpc: client, interactions: make(map[string]piInteraction), events: make(chan provider.Event, 1)}
	requestID := behaviorID(87)
	deadline := now.Add(20 * time.Millisecond)
	request := provider.InteractionRequest{ID: requestID, TurnID: behaviorID(88), Kind: provider.InteractionUserInput, Title: "Choose", Questions: []provider.InteractionQuestion{{ID: "value", Header: "Choose", Prompt: "Choose", Options: []provider.InteractionOption{{ID: "yes", Label: "Yes"}}}}, LocalDeadline: &deadline}
	session.interactions[requestID] = piInteraction{nativeID: "native", method: "select", request: request, choices: map[string]string{"yes": "Yes"}}
	answer := make(chan error, 1)
	go func() {
		answer <- session.Respond(context.Background(), provider.InteractionResponse{RequestID: requestID, Kind: request.Kind, Answers: map[string][]string{"value": {"yes"}}})
	}()
	<-input.entered
	select {
	case err := <-answer:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("Respond did not bound the complete blocked write")
	}
	session.mu.Lock()
	_, retained := session.interactions[requestID]
	session.mu.Unlock()
	require.False(t, retained)
	select {
	case event := <-session.events:
		t.Fatalf("unknown write outcome emitted provider resolution: %#v", event)
	default:
	}
	_ = input.Close()
	_ = outputWriter.Close()
}

func TestPiBrokerCancellationWritesCompletelyWithoutProviderResolution(t *testing.T) {
	session, child := newBehaviorSession(t)
	requestID := behaviorID(92)
	request := provider.InteractionRequest{ID: requestID, TurnID: behaviorID(93), Kind: provider.InteractionUserInput, Title: "Choose", Questions: []provider.InteractionQuestion{{ID: "value", Header: "Choose", Prompt: "Choose", Options: []provider.InteractionOption{{ID: "yes", Label: "Yes"}}}}}
	session.mu.Lock()
	session.interactions[requestID] = piInteraction{nativeID: "native-cancel", method: "select", request: request, choices: map[string]string{"yes": "Yes"}}
	session.mu.Unlock()
	answer := make(chan error, 1)
	go func() { answer <- session.CancelInteraction(context.Background(), requestID) }()
	wire := child.readCommand(t)
	require.Equal(t, "extension_ui_response", wire["type"])
	require.Equal(t, "native-cancel", wire["id"])
	require.Equal(t, true, wire["cancelled"])
	require.NoError(t, <-answer)
	select {
	case event := <-session.Events():
		t.Fatalf("broker cancellation emitted provider resolution: %#v", event)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestPiInteractionLocalExpiryConsumesOwnershipWithoutNativeWrite(t *testing.T) {
	session, _ := newBehaviorSession(t)
	requestID := behaviorID(79)
	deadline := session.driver.config.Clock.Now().UTC().Add(-time.Millisecond)
	request := provider.InteractionRequest{ID: requestID, TurnID: behaviorID(80), Kind: provider.InteractionUserInput, Title: "Choose", Questions: []provider.InteractionQuestion{{ID: "value", Header: "Choose", Prompt: "Choose", Options: []provider.InteractionOption{{ID: "yes", Label: "Yes"}}}}, LocalDeadline: &deadline}
	session.mu.Lock()
	session.interactions[requestID] = piInteraction{nativeID: "native", method: "select", request: request, choices: map[string]string{"yes": "Yes"}}
	session.mu.Unlock()
	err := session.Respond(context.Background(), provider.InteractionResponse{RequestID: requestID, Kind: request.Kind, Answers: map[string][]string{"value": {"yes"}}})
	assertProviderCode(t, err, provider.ErrorProtocolFailure)
	resolved := receiveProviderEvents(t, session.Events(), 1)[0]
	require.Equal(t, provider.EventInteractionResolved, resolved.Kind)
	require.Empty(t, session.rpc.writes)
}

type manualTimerFactory struct {
	mu     sync.Mutex
	timers []*manualOneShotTimer
}

type manualOneShotTimer struct {
	mu       sync.Mutex
	callback func()
	stopped  bool
}

func (factory *manualTimerFactory) AfterFunc(_ time.Duration, callback func()) OneShotTimer {
	timer := &manualOneShotTimer{callback: callback}
	factory.mu.Lock()
	factory.timers = append(factory.timers, timer)
	factory.mu.Unlock()
	return timer
}

func (factory *manualTimerFactory) timer(t *testing.T, index int) *manualOneShotTimer {
	t.Helper()
	factory.mu.Lock()
	defer factory.mu.Unlock()
	require.Greater(t, len(factory.timers), index)
	return factory.timers[index]
}

func (timer *manualOneShotTimer) Stop() bool {
	timer.mu.Lock()
	defer timer.mu.Unlock()
	wasActive := !timer.stopped
	timer.stopped = true
	return wasActive
}

func (timer *manualOneShotTimer) Fire() {
	timer.mu.Lock()
	if timer.stopped {
		timer.mu.Unlock()
		return
	}
	callback := timer.callback
	timer.mu.Unlock()
	callback()
}

func (timer *manualOneShotTimer) Stopped() bool {
	timer.mu.Lock()
	defer timer.mu.Unlock()
	return timer.stopped
}

func newManualTimedInteraction(t *testing.T, seed byte) (*Session, *rpcFakeChild, *manualTimerFactory, provider.InteractionRequest) {
	t.Helper()
	session, child := newBehaviorSession(t)
	timers := &manualTimerFactory{}
	session.driver.config.Timers = timers
	session.driver.config.IDs = fixedIDs{value: behaviorID(seed)}
	installBehaviorTurn(session, seed+1)
	child.writeRecord(t, map[string]any{"type": "extension_ui_request", "id": "native-timed", "method": "select", "title": "Choose", "options": []string{"Allow"}, "timeout": 10000})
	event := receiveProviderEvents(t, session.Events(), 1)[0]
	require.Equal(t, provider.EventInteractionRequest, event.Kind)
	require.NotNil(t, event.Interaction.LocalDeadline)
	require.Len(t, timers.timers, 1)
	return session, child, timers, *event.Interaction
}

func finishPiShutdown(t *testing.T, session *Session, child *rpcFakeChild, shutdown <-chan error) []provider.Event {
	t.Helper()
	child.closeOutput()
	require.NoError(t, <-shutdown)
	events := make([]provider.Event, 0)
	for event := range session.Events() {
		events = append(events, event)
	}
	return events
}

func countPiInteractionResolutions(events []provider.Event, requestID string) int {
	count := 0
	for _, event := range events {
		if event.Kind == provider.EventInteractionResolved && event.Resolution.RequestID == requestID {
			count++
		}
	}
	return count
}

func assertNoPiInteractionEvent(t *testing.T, events <-chan provider.Event) {
	t.Helper()
	select {
	case event := <-events:
		t.Fatalf("unexpected interaction event: %#v", event)
	case <-time.After(20 * time.Millisecond):
	}
}

func newPersistentBehaviorSession(t *testing.T) (*Session, *rpcFakeChild, *nativeManager) {
	t.Helper()
	manager, workspace := newTestNativeManager(t)
	allocation, err := manager.allocate(workspace)
	require.NoError(t, err)
	writeNativeHeader(t, allocation, "session")
	state := startupState{SessionID: "session", SessionFile: allocation.path, Workspace: workspace, ModelProvider: "model-provider", ModelID: "model-id", Model: "model-provider/model-id", ModelName: "Current", ThinkingLevel: "off", ContextWindow: 32768, MaxTokens: 1024}
	native, err := manager.finalizeAllocation(allocation, state)
	require.NoError(t, err)
	child := newRPCFakeChild()
	client, err := newRPCClient(child)
	require.NoError(t, err)
	driver := &Driver{config: Config{IDs: fixedIDs{value: behaviorID(90)}, Clock: fixedClock{value: time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC)}}, native: manager, active: make(map[string]provider.Session), unfinalized: make(map[string]nativeAllocation)}
	session := newSession(driver, native, state, child, client)
	driver.active[native.Ref.Value()] = session
	session.start()
	t.Cleanup(func() {
		child.closeOutput()
		select {
		case <-session.loopsDone:
		case <-time.After(time.Second):
			t.Errorf("persistent session loops did not stop")
		}
	})
	return session, child, manager
}

func respondPiSettingsCatalog(t *testing.T, child *rpcFakeChild) {
	t.Helper()
	catalog := child.readCommand(t)
	require.Equal(t, "get_available_models", catalog["type"])
	child.writeRecord(t, responseRecord(catalog, map[string]any{"models": []any{
		map[string]any{"provider": "model-provider", "id": "model-id", "name": "Current", "reasoning": true, "input": []string{"text"}, "contextWindow": 32768, "maxTokens": 1024},
		map[string]any{"provider": "model-provider", "id": "next", "name": "Next", "reasoning": true, "input": []string{"text", "image"}, "contextWindow": 32768, "maxTokens": 1024},
	}}))
}

func respondPiEffectiveState(t *testing.T, child *rpcFakeChild, session *Session, modelID, modelName, effort string) {
	t.Helper()
	state := child.readCommand(t)
	require.Equal(t, "get_state", state["type"])
	respondPiStateCommand(t, child, session, state, modelID, modelName, effort)
}

func respondPiStateCommand(t *testing.T, child *rpcFakeChild, session *Session, state map[string]any, modelID, modelName, effort string) {
	t.Helper()
	session.mu.Lock()
	sessionFile, sessionID := session.state.SessionFile, session.state.SessionID
	session.mu.Unlock()
	child.writeRecord(t, responseRecord(state, map[string]any{"model": map[string]any{"provider": "model-provider", "id": modelID, "name": modelName, "contextWindow": 32768, "maxTokens": 1024, "input": []string{"text"}}, "thinkingLevel": effort, "isStreaming": false, "isCompacting": false, "pendingMessageCount": 0, "sessionFile": sessionFile, "sessionId": sessionID}))
}

func respondPiBusyStateCommand(t *testing.T, child *rpcFakeChild, session *Session, state map[string]any, streaming, compacting bool, pendingCount int) {
	t.Helper()
	session.mu.Lock()
	sessionFile, sessionID := session.state.SessionFile, session.state.SessionID
	session.mu.Unlock()
	child.writeRecord(t, responseRecord(state, map[string]any{"model": map[string]any{"provider": "model-provider", "id": "next", "name": "Next", "contextWindow": 32768, "maxTokens": 1024, "input": []string{"text"}}, "thinkingLevel": "high", "isStreaming": streaming, "isCompacting": compacting, "pendingMessageCount": pendingCount, "sessionFile": sessionFile, "sessionId": sessionID}))
}

func effortValues(efforts []provider.ReasoningEffort) []string {
	result := make([]string, len(efforts))
	for i, e := range efforts {
		result[i] = e.Value
	}
	return result
}
