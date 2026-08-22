package protocol_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	"github.com/stretchr/testify/require"
)

func TestV5SettingsCommandsAreStrictCompleteAndProviderSpecific(t *testing.T) {
	require.Equal(t, "5", protocol.APIVersion)
	require.Equal(t, "agent-whiteboard.v5", protocol.WebSocketSubprotocol)

	codexConnect := `{"api_version":"5","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":null,"type":"connect","payload":{"provider":"codex","resource":{"kind":"markdown","id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","created_at":"2026-07-27T01:02:03Z","updated_at":"2026-07-27T02:03:04Z","expires_at":null},"context_digest":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","settings":{"model":"gpt-5.6-sol","effort":"high","speed":"fast"}}}`
	decoded, err := protocol.DecodeCommand([]byte(codexConnect))
	require.NoError(t, err)
	connect := decoded.Payload.(protocol.ConnectPayload)
	require.NotNil(t, connect.Settings)
	require.Equal(t, protocol.SpeedFast, connect.Settings.Speed)
	encoded, err := protocol.EncodeCommand(decoded)
	require.NoError(t, err)
	require.JSONEq(t, codexConnect, string(encoded))

	piConnect := strings.Replace(codexConnect, `"provider":"codex"`, `"provider":"pi"`, 1)
	piConnect = strings.Replace(piConnect, `"settings":{"model":"gpt-5.6-sol","effort":"high","speed":"fast"}`, `"settings":null`, 1)
	_, err = protocol.DecodeCommand([]byte(piConnect))
	require.NoError(t, err)
	_, err = protocol.DecodeCommand([]byte(strings.Replace(piConnect, `"settings":null`, `"settings":{"model":"gpt-5.6-sol","effort":"high","speed":"fast"}`, 1)))
	require.ErrorIs(t, err, protocol.ErrInvalidMessage)

	conversationID := idC
	settings := protocol.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "high", Speed: protocol.SpeedFast}
	commands := []protocol.Command{
		{APIVersion: protocol.APIVersion, CommandID: idA, ClientID: idB, ConversationID: &conversationID, Type: protocol.CommandSubmit, Payload: protocol.SubmitPayload{TurnID: makeID(200), MessageID: makeID(201), Content: protocol.TextContent("question"), Settings: &settings}},
		{APIVersion: protocol.APIVersion, CommandID: idA, ClientID: idB, ConversationID: &conversationID, Type: protocol.CommandNew, Payload: protocol.NewPayload{Settings: &settings}},
	}
	for _, command := range commands {
		encoded, err := protocol.EncodeCommand(command)
		require.NoError(t, err)
		decoded, err := protocol.DecodeCommand(encoded)
		require.NoError(t, err)
		require.Equal(t, command, decoded)
	}
	standard := commands[0]
	standardSettings := settings
	standardSettings.Speed = protocol.SpeedStandard
	standard.Payload = protocol.SubmitPayload{TurnID: makeID(200), MessageID: makeID(201), Content: protocol.TextContent("question"), Settings: &standardSettings}
	fastBytes, err := protocol.EncodeCommand(commands[0])
	require.NoError(t, err)
	standardBytes, err := protocol.EncodeCommand(standard)
	require.NoError(t, err)
	require.NotEqual(t, fastBytes, standardBytes, "canonical command bytes must distinguish captured settings for command fingerprints")
}

func TestV5SettingsCommandsRejectMissingPartialDuplicateNullAndNativeValues(t *testing.T) {
	valid := `{"api_version":"5","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"new","payload":{"settings":{"model":"gpt-5.6-sol","effort":"high","speed":"fast"}}}`
	cases := map[string]string{
		"missing settings":    strings.Replace(valid, `"settings":{"model":"gpt-5.6-sol","effort":"high","speed":"fast"}`, ``, 1),
		"partial settings":    strings.Replace(valid, `,"effort":"high"`, ``, 1),
		"duplicate nested":    strings.Replace(valid, `"model":"gpt-5.6-sol"`, `"model":"gpt-5.6-sol","model":"other"`, 1),
		"null required model": strings.Replace(valid, `"model":"gpt-5.6-sol"`, `"model":null`, 1),
		"native speed":        strings.Replace(valid, `"speed":"fast"`, `"speed":"priority"`, 1),
		"oversized model":     strings.Replace(valid, `gpt-5.6-sol`, strings.Repeat("x", protocol.MaxModelValueBytes+1), 1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := protocol.DecodeCommand([]byte(input))
			require.ErrorIs(t, err, protocol.ErrInvalidMessage)
		})
	}

	nullSettings := strings.Replace(valid, `{"model":"gpt-5.6-sol","effort":"high","speed":"fast"}`, `null`, 1)
	_, err := protocol.DecodeCommand([]byte(nullSettings))
	require.NoError(t, err, "nullable settings represent Pi/non-configurable providers")
}

func TestV5QueueSnapshotAndSettingsEventsCarryBoundedPresentationAndCatalog(t *testing.T) {
	settings := validPresentedSettings()
	catalog := validProtocolCatalog()
	state := protocol.SettingsVerified
	acceptedTurnID := idA
	queue := protocol.QueueItem{TurnID: idA, MessageID: idB, Content: protocol.TextContent("queued"), Settings: &settings}

	payloads := []protocol.EventPayload{
		protocol.QueuePayload{Items: []protocol.QueueItem{queue}},
		protocol.SnapshotPayload{
			Lifecycle: protocol.LifecycleReady, Queue: []protocol.QueueItem{queue}, ContextState: protocol.ContextAccepted, SupportsImages: true,
			SettingsState: &state, EffectiveSettings: &settings, Catalog: catalog, SkillsState: &readySkillsState, Skills: []protocol.SkillDescriptor{}, MaxSelectedSkills: &maxSelectedSkills, BusyPolicy: protocol.BusyTurnQueue, ComposerAdmission: protocol.ComposerSubmit,
		},
		protocol.SettingsPayload{SettingsState: protocol.SettingsVerified, EffectiveSettings: &settings, Catalog: catalog, AcceptedTurnID: &acceptedTurnID},
		protocol.SettingsPayload{SettingsState: protocol.SettingsUnverified, Catalog: catalog},
	}
	for _, payload := range payloads {
		event := validEvent(payload)
		encoded, err := protocol.EncodeEvent(event)
		require.NoError(t, err, "%T", payload)
		decoded, err := protocol.DecodeEvent(encoded)
		require.NoError(t, err, "%T: %s", payload, encoded)
		require.Equal(t, event, decoded)
	}

	piSnapshot := protocol.SnapshotPayload{Lifecycle: protocol.LifecycleReady, Queue: []protocol.QueueItem{}, ContextState: protocol.ContextAccepted, Catalog: []protocol.CatalogModel{}, SkillsState: &readySkillsState, Skills: []protocol.SkillDescriptor{}, MaxSelectedSkills: &maxSelectedSkills, BusyPolicy: protocol.BusyTurnQueue, ComposerAdmission: protocol.ComposerSubmit}
	encoded, err := protocol.EncodeEvent(validEvent(piSnapshot))
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"settings_state":null`)
	require.Contains(t, string(encoded), `"effective_settings":null`)
}

func TestV5SettingsEventsRejectContradictoryAndMalformedState(t *testing.T) {
	settings := validPresentedSettings()
	catalog := validProtocolCatalog()
	state := protocol.SettingsVerified

	incompatibleEffort := settings
	incompatibleEffort.Effort = "xhigh"
	incompatibleFast := settings
	incompatibleFast.Model = "gpt-5.6-luna"
	incompatibleFast.ModelDisplayName = "5.6 Luna"
	incompatibleFast.Effort = "medium"
	catalogWithLuna := append(validProtocolCatalog(), protocol.CatalogModel{
		Model: "gpt-5.6-luna", ModelDisplayName: "5.6 Luna", Description: "Plan execution", DefaultEffort: "medium",
		SupportedReasoningEfforts: []protocol.ReasoningEffortOption{{Effort: "medium", Description: "Balanced"}},
	})
	catalogWithLuna[0].Default = false
	catalogWithLuna[1].Default = true
	invalid := []protocol.EventPayload{
		protocol.SettingsPayload{SettingsState: protocol.SettingsVerified, Catalog: catalog},
		protocol.SettingsPayload{SettingsState: protocol.SettingsUnverified, EffectiveSettings: &settings, Catalog: catalog},
		protocol.SettingsPayload{SettingsState: protocol.SettingsVerified, EffectiveSettings: &settings, Catalog: nil},
		protocol.SettingsPayload{SettingsState: protocol.SettingsVerified, EffectiveSettings: &incompatibleEffort, Catalog: catalog},
		protocol.SettingsPayload{SettingsState: protocol.SettingsVerified, EffectiveSettings: &incompatibleFast, Catalog: catalogWithLuna},
		protocol.SnapshotPayload{Lifecycle: protocol.LifecycleReady, Queue: []protocol.QueueItem{}, ContextState: protocol.ContextAccepted, SettingsState: &state, Catalog: catalog, SkillsState: &readySkillsState, Skills: []protocol.SkillDescriptor{}, MaxSelectedSkills: &maxSelectedSkills, BusyPolicy: protocol.BusyTurnQueue, ComposerAdmission: protocol.ComposerSubmit},
		protocol.SnapshotPayload{Lifecycle: protocol.LifecycleReady, Queue: []protocol.QueueItem{}, ContextState: protocol.ContextAccepted, SettingsState: &state, EffectiveSettings: &settings, Catalog: catalog, SkillsState: &readySkillsState, Skills: []protocol.SkillDescriptor{}, MaxSelectedSkills: &maxSelectedSkills, BusyPolicy: protocol.BusyTurnQueue, ComposerAdmission: protocol.ComposerSubmit},
	}
	for _, payload := range invalid {
		_, err := protocol.EncodeEvent(validEvent(payload))
		require.Error(t, err, "%T", payload)
	}

	valid := validEvent(protocol.SettingsPayload{SettingsState: protocol.SettingsVerified, EffectiveSettings: &settings, Catalog: catalog})
	encoded, err := protocol.EncodeEvent(valid)
	require.NoError(t, err)
	for name, input := range map[string]string{
		"missing state":           strings.Replace(string(encoded), `"settings_state":"verified",`, ``, 1),
		"partial effective":       strings.Replace(string(encoded), `,"effort":"high"`, ``, 1),
		"duplicate catalog model": strings.Replace(string(encoded), `"model":"gpt-5.6-sol"`, `"model":"gpt-5.6-sol","model":"other"`, 1),
		"native tier leak":        strings.Replace(string(encoded), `"speed":"fast"`, `"speed":"priority"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := protocol.DecodeEvent([]byte(input))
			require.ErrorIs(t, err, protocol.ErrInvalidMessage)
		})
	}
}

func TestV5CatalogWorstCaseFitsMeasuredEventBound(t *testing.T) {
	catalog := make([]protocol.CatalogModel, 0, protocol.MaxCatalogModels)
	remaining := protocol.MaxCatalogBytes
	for index := 0; index < protocol.MaxCatalogModels && remaining > 0; index++ {
		model := fmt.Sprintf("model-%03d", index)
		display := fmt.Sprintf("Model %03d", index)
		effort := fmt.Sprintf("effort-%03d", index)
		fixed := len(model) + len(display) + len(effort)*2 + len("supported")
		descriptionBytes := min(protocol.MaxModelDescriptionBytes, max(0, remaining-fixed))
		if descriptionBytes == 0 {
			break
		}
		catalog = append(catalog, protocol.CatalogModel{
			Model: model, ModelDisplayName: display, Description: exactBytes(`"\`, descriptionBytes), DefaultEffort: effort,
			SupportedReasoningEfforts: []protocol.ReasoningEffortOption{{Effort: effort, Description: "supported"}},
			SupportsFast:              true, Default: index == 0,
		})
		remaining -= fixed + descriptionBytes
	}
	require.NotEmpty(t, catalog)
	queue := make([]protocol.QueueItem, protocol.MaxQueueItems)
	settingsBytes := protocol.MaxModelValueBytes + protocol.MaxEffortValueBytes + len(protocol.SpeedFast) + protocol.MaxTitleBytes
	contentRemaining := protocol.MaxQueueBytes - settingsBytes*len(queue)
	for index := range queue {
		captured := protocol.PresentedExecutionSettings{
			ExecutionSettings: protocol.ExecutionSettings{
				Model: exactBytes(`"\\`, protocol.MaxModelValueBytes), Effort: exactBytes(`"\\`, protocol.MaxEffortValueBytes), Speed: protocol.SpeedFast,
			},
			ModelDisplayName: exactBytes(`"\\`, protocol.MaxTitleBytes), Selectable: false,
		}
		contentBytes := contentRemaining / (len(queue) - index)
		contentRemaining -= contentBytes
		queue[index] = protocol.QueueItem{TurnID: makeID(byte(index)), MessageID: makeID(byte(index + protocol.MaxQueueItems)), Content: protocol.TextContent(exactBytes(`"\\`, contentBytes)), Settings: &captured}
	}
	require.NoError(t, protocol.ValidateQueue(queue))
	settings := protocol.PresentedExecutionSettings{ExecutionSettings: protocol.ExecutionSettings{Model: catalog[0].Model, Effort: catalog[0].DefaultEffort, Speed: protocol.SpeedFast}, ModelDisplayName: catalog[0].ModelDisplayName, Selectable: true}
	state := protocol.SettingsVerified
	payload := protocol.SnapshotPayload{Lifecycle: protocol.LifecycleReady, Queue: queue, ContextState: protocol.ContextAccepted, SettingsState: &state, EffectiveSettings: &settings, Catalog: catalog, SkillsState: &readySkillsState, Skills: []protocol.SkillDescriptor{}, MaxSelectedSkills: &maxSelectedSkills, BusyPolicy: protocol.BusyTurnQueue, ComposerAdmission: protocol.ComposerSubmit}
	encoded, err := protocol.EncodeEvent(validEvent(payload))
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), protocol.MaxEventBytes)
	_, err = protocol.DecodeEvent(encoded)
	require.NoError(t, err)
}

func TestV5InvalidModelConfigurationBrowserErrorIsClosed(t *testing.T) {
	err := protocol.NewBrowserError(protocol.ErrorInvalidModelConfiguration)
	require.Equal(t, protocol.ErrorInvalidModelConfiguration, err.Code())
	require.Equal(t, protocol.ActionConfigureModel, err.Action())
	require.Contains(t, protocol.AllBrowserErrorCodes(), protocol.ErrorInvalidModelConfiguration)
	encoded, marshalErr := json.Marshal(err)
	require.NoError(t, marshalErr)
	require.NotContains(t, string(encoded), "gpt-")
}

func validPresentedSettings() protocol.PresentedExecutionSettings {
	return protocol.PresentedExecutionSettings{
		ExecutionSettings: protocol.ExecutionSettings{Model: "gpt-5.6-sol", Effort: "high", Speed: protocol.SpeedFast},
		ModelDisplayName:  "5.6 Sol", Selectable: true,
	}
}

func validProtocolCatalog() []protocol.CatalogModel {
	return []protocol.CatalogModel{{
		Model: "gpt-5.6-sol", ModelDisplayName: "5.6 Sol", Description: "Deep coding work", DefaultEffort: "medium",
		SupportedReasoningEfforts: []protocol.ReasoningEffortOption{{Effort: "low", Description: "Quick"}, {Effort: "medium", Description: "Balanced"}, {Effort: "high", Description: "Deep"}},
		SupportsImages:            true, Default: true, SupportsFast: true,
	}}
}
