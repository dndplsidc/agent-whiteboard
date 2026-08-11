package protocol_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent"
	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	"github.com/stretchr/testify/require"
)

const (
	idA    = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	idB    = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	idC    = "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
	digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func TestDecodeConnectCommandContract(t *testing.T) {
	input := `{"api_version":"3","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":null,"type":"connect","payload":{"provider":"pi","resource":{"kind":"markdown","id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","created_at":"2026-07-27T01:02:03Z","updated_at":"2026-07-27T02:03:04Z","expires_at":null},"context_digest":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","replay_after":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"}}`

	command, err := protocol.DecodeCommand([]byte(input))
	require.NoError(t, err)
	require.Equal(t, protocol.CommandConnect, command.Type)
	require.Nil(t, command.ConversationID)
	payload, ok := command.Payload.(protocol.ConnectPayload)
	require.True(t, ok)
	require.Equal(t, protocol.ProviderPi, payload.Provider)
	require.Equal(t, protocol.ResourceMarkdown, payload.Resource.Kind)
	require.Equal(t, idC, payload.Resource.ID)
	require.Equal(t, digest, payload.ContextDigest)
	require.Equal(t, "DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD", payload.ReplayAfter)

	encoded, err := json.Marshal(command)
	require.NoError(t, err)
	require.JSONEq(t, input, string(encoded))
	require.NotContains(t, string(encoded), `"source":`)
	require.NotContains(t, string(encoded), `"context":`)
	require.NotContains(t, string(encoded), "title")
}

func TestProviderNamesAreClosedAndCodexConnectRoundTrips(t *testing.T) {
	require.True(t, protocol.ProviderPi.Valid())
	require.True(t, protocol.ProviderCodex.Valid())
	require.False(t, protocol.ProviderName("other").Valid())
	require.Equal(t, []protocol.ProviderName{protocol.ProviderPi, protocol.ProviderCodex}, protocol.AllProviderNames())

	input := `{"api_version":"3","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":null,"type":"connect","payload":{"provider":"codex","resource":{"kind":"markdown","id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","created_at":"2026-07-27T01:02:03Z","updated_at":"2026-07-27T02:03:04Z","expires_at":null},"context_digest":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}}`
	command, err := protocol.DecodeCommand([]byte(input))
	require.NoError(t, err)
	require.Equal(t, protocol.ProviderCodex, command.Payload.(protocol.ConnectPayload).Provider)
	encoded, err := protocol.EncodeCommand(command)
	require.NoError(t, err)
	require.JSONEq(t, input, string(encoded))
}

func TestDecodeSubmitCarriesCompleteInitialOrReplacementContext(t *testing.T) {
	for _, revision := range []protocol.ContextRevision{protocol.ContextInitial, protocol.ContextReplacement} {
		t.Run(string(revision), func(t *testing.T) {
			command := validSubmitJSON(string(revision))
			decoded, err := protocol.DecodeCommand([]byte(command))
			require.NoError(t, err)
			payload := decoded.Payload.(protocol.SubmitPayload)
			require.Equal(t, protocol.TextContent("question"), payload.Content)
			require.NotNil(t, payload.Context)
			require.Equal(t, "# page", payload.Context.Markdown)
			require.Equal(t, "creator summary", payload.Context.CreatorContext)
			require.Equal(t, revision, payload.Context.Revision)
		})
	}

	withoutContext := `{"api_version":"3","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"submit","payload":{"turn_id":"EEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE","message_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD","content":{"parts":[{"type":"text","text":"follow up"}]}}}`
	decoded, err := protocol.DecodeCommand([]byte(withoutContext))
	require.NoError(t, err)
	payload := decoded.Payload.(protocol.SubmitPayload)
	require.Equal(t, "EEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE", payload.TurnID)
	require.Nil(t, payload.Context)
}

func TestAllCommandPayloadsAreClosedAndValidated(t *testing.T) {
	commands := []string{
		`{"api_version":"3","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"queue_edit","payload":{"message_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD","content":{"parts":[{"type":"text","text":"edited"}]}}}`,
		`{"api_version":"3","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"queue_remove","payload":{"message_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"}}`,
		`{"api_version":"3","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"interrupt","payload":{"turn_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"}}`,
		`{"api_version":"3","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"retry","payload":{"turn_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"}}`,
		`{"api_version":"3","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"new","payload":{}}`,
		`{"api_version":"3","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"archive_list","payload":{"limit":50}}`,
		`{"api_version":"3","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"archive_restore","payload":{"archive_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"}}`,
		`{"api_version":"3","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"archive_delete","payload":{"archive_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"}}`,
		`{"api_version":"3","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"history_page","payload":{"before":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD","limit":100}}`,
		`{"api_version":"3","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"resync","payload":{"after_event_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"}}`,
	}
	for _, input := range commands {
		decoded, err := protocol.DecodeCommand([]byte(input))
		require.NoError(t, err, input)
		require.NotNil(t, decoded.Payload)
	}
}

func TestInteractionResponseCommandIsStrictAndBounded(t *testing.T) {
	input := `{"api_version":"3","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"interaction_respond","payload":{"request_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD","kind":"user_input","option_id":"","answers":{"target":["local"]}}}`
	command, err := protocol.DecodeCommand([]byte(input))
	require.NoError(t, err)
	payload := command.Payload.(protocol.InteractionResponsePayload)
	require.Equal(t, protocol.InteractionUserInput, payload.Kind)
	require.Equal(t, []string{"local"}, payload.Answers["target"])
	encoded, err := protocol.EncodeCommand(command)
	require.NoError(t, err)
	require.JSONEq(t, input, string(encoded))

	bad := strings.Replace(input, `"request_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"`, `"request_id":"native"`, 1)
	_, err = protocol.DecodeCommand([]byte(bad))
	require.Error(t, err)
}

func TestToolAndInteractionEventsRoundTripWithoutNativeIdentifiers(t *testing.T) {
	tool := protocol.ToolActivityPayload{
		ActivityID: idC, TurnID: idA, Kind: protocol.ToolCommand, Status: protocol.ToolRunning,
		Title: "Run tests", Summary: "Running tests", Detail: "go test ./internal/agent/codex",
	}
	request := protocol.InteractionRequestPayload{
		RequestID: idC, TurnID: idA, Kind: protocol.InteractionCommandApproval,
		Title: "Approve command", Summary: "Run tests?", Command: "go test ./internal/agent/codex", WorkingDirectory: "/workspace",
		Options: []protocol.InteractionOption{{ID: "accept", Label: "Allow once", Description: "Run it."}},
	}
	resolved := protocol.InteractionResolvedPayload{RequestID: idC, Kind: protocol.InteractionCommandApproval, OptionID: "accept"}
	for _, payload := range []protocol.EventPayload{tool, request, resolved} {
		event := validEvent(payload)
		encoded, err := protocol.EncodeEvent(event)
		require.NoError(t, err)
		require.NotContains(t, string(encoded), "native")
		decoded, err := protocol.DecodeEvent(encoded)
		require.NoError(t, err)
		require.Equal(t, event, decoded)
	}
}

func TestCommandStrictDecoding(t *testing.T) {
	valid := `{"api_version":"3","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"queue_remove","payload":{"message_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"}}`
	tests := map[string]string{
		"unknown envelope field":     strings.Replace(valid, `"payload":`, `"extra":1,"payload":`, 1),
		"duplicate envelope field":   strings.Replace(valid, `"type":`, `"type":"queue_remove","type":`, 1),
		"unknown payload field":      strings.Replace(valid, `"message_id":`, `"extra":1,"message_id":`, 1),
		"duplicate payload field":    strings.Replace(valid, `"message_id":`, `"message_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD","message_id":`, 1),
		"trailing value":             valid + `{}`,
		"trailing malformed value":   valid + `x`,
		"missing conversation field": strings.Replace(valid, `"conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC",`, ``, 1),
		"unexpected null":            strings.Replace(valid, `"message_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"`, `"message_id":null`, 1),
		"wrong payload":              strings.Replace(valid, `"message_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"`, `"turn_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"`, 1),
		"bad version":                strings.Replace(valid, `"api_version":"3"`, `"api_version":"2"`, 1),
		"bad id":                     strings.Replace(valid, idA, "short", 1),
		"bad enum":                   strings.Replace(valid, `"type":"queue_remove"`, `"type":"native_raw"`, 1),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := protocol.DecodeCommand([]byte(input))
			require.ErrorIs(t, err, protocol.ErrInvalidMessage)
		})
	}

	invalidUTF8 := append([]byte(valid[:len(valid)-1]), 0xff, '}')
	_, err := protocol.DecodeCommand(invalidUTF8)
	require.ErrorIs(t, err, protocol.ErrInvalidMessage)
}

func TestConnectRejectsMalformedNestedSecurityValues(t *testing.T) {
	valid := `{"api_version":"3","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":null,"type":"connect","payload":{"provider":"pi","resource":{"kind":"markdown","id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","created_at":"2026-07-27T01:02:03Z","updated_at":"2026-07-27T02:03:04Z","expires_at":null},"context_digest":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}}`
	tests := map[string]string{
		"nested duplicate":    strings.Replace(valid, `"kind":"markdown"`, `"kind":"markdown","kind":"markdown"`, 1),
		"uppercase digest":    strings.Replace(valid, digest, strings.ToUpper(digest), 1),
		"malformed timestamp": strings.Replace(valid, "2026-07-27T01:02:03Z", "yesterday", 1),
		"wrong resource kind": strings.Replace(valid, `"kind":"markdown"`, `"kind":"html"`, 1),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := protocol.DecodeCommand([]byte(input))
			require.ErrorIs(t, err, protocol.ErrInvalidMessage)
		})
	}

	loopbackURL := strings.Replace(validSubmitJSON("initial"), "https://whiteboard.example/", "http://127.0.0.1:8080/", 1)
	_, err := protocol.DecodeCommand([]byte(loopbackURL))
	require.NoError(t, err)
	for _, origin := range []string{"file://", "http://localhost:8080", "http://127.0.0.1:80", "http://127.0.0.1:080", "http://127.0.0.2:8080", "http://[::1]:8080", "http://user@127.0.0.1:8080"} {
		badURL := strings.Replace(validSubmitJSON("initial"), "https://whiteboard.example", origin, 1)
		_, err = protocol.DecodeCommand([]byte(badURL))
		require.ErrorIs(t, err, protocol.ErrInvalidMessage, origin)
	}
}

func TestCommandBoundsAreMeasuredInBytes(t *testing.T) {
	ordinary := `{"api_version":"3","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"queue_edit","payload":{"message_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD","content":{"parts":[{"type":"text","text":"` + strings.Repeat("é", protocol.MaxMessageBytes/2+1) + `"}]}}}`
	_, err := protocol.DecodeCommand([]byte(ordinary))
	require.ErrorIs(t, err, protocol.ErrInvalidMessage)

	context := validSubmitJSON("initial")
	context = strings.Replace(context, `"# page"`, `"`+strings.Repeat("x", protocol.MaxContextCommandBytes)+`"`, 1)
	_, err = protocol.DecodeCommand([]byte(context))
	require.ErrorIs(t, err, protocol.ErrMessageTooLarge)

	tooLongTitle := strings.Replace(validSubmitJSON("initial"), `"Page title"`, `"`+strings.Repeat("t", protocol.MaxTitleBytes+1)+`"`, 1)
	_, err = protocol.DecodeCommand([]byte(tooLongTitle))
	require.ErrorIs(t, err, protocol.ErrInvalidMessage)

	tooLongURL := strings.Replace(validSubmitJSON("initial"), `https://whiteboard.example/whiteboards/markdown/CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC`, `https://whiteboard.example/`+strings.Repeat("u", protocol.MaxURLBytes), 1)
	_, err = protocol.DecodeCommand([]byte(tooLongURL))
	require.ErrorIs(t, err, protocol.ErrInvalidMessage)
}

func TestAcceptedExactBoundMessagesAndQueuesAlwaysFitWireFrames(t *testing.T) {
	require.Equal(t, 64<<10, protocol.MaxMessageBytes)
	require.Equal(t, 192<<10, protocol.MaxOrdinaryCommandBytes)
	require.Equal(t, 96<<10, protocol.MaxQueueBytes)
	require.Equal(t, 256<<10, protocol.MaxEventBytes)

	conversationID := idC
	worstCharacters := []string{"\t", "\n", "\r", `"`, `\`, "\u2028"}
	for _, character := range worstCharacters {
		t.Run(fmt.Sprintf("message_%U", []rune(character)[0]), func(t *testing.T) {
			message := exactBytes(character, protocol.MaxMessageBytes)
			command := protocol.Command{
				APIVersion: protocol.APIVersion, CommandID: idA, ClientID: idB, ConversationID: &conversationID, Type: protocol.CommandQueueEdit,
				Payload: protocol.QueueEditPayload{MessageID: makeID(200), Content: protocol.TextContent(message)},
			}
			encodedCommand, err := protocol.EncodeCommand(command)
			require.NoError(t, err)
			require.LessOrEqual(t, len(encodedCommand), protocol.MaxOrdinaryCommandBytes)
			_, err = protocol.DecodeCommand(encodedCommand)
			require.NoError(t, err)

			event := validEvent(protocol.AssistantMessagePayload{TurnID: idC, MessageID: idA, Text: message, CreatedAt: time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)})
			encodedEvent, err := protocol.EncodeEvent(event)
			require.NoError(t, err)
			require.LessOrEqual(t, len(encodedEvent), protocol.MaxEventBytes)
		})
	}

	queue := make([]protocol.QueueItem, protocol.MaxQueueItems)
	remaining := protocol.MaxQueueBytes
	for index := range queue {
		size := remaining / (len(queue) - index)
		if index == 0 {
			size = protocol.MaxMessageBytes
		}
		remaining -= size
		queue[index] = protocol.QueueItem{TurnID: makeID(byte(index)), MessageID: makeID(byte(index + protocol.MaxQueueItems)), Content: protocol.TextContent(exactBytes(`"\`, size))}
	}
	require.NoError(t, protocol.ValidateQueue(queue))
	for _, payload := range []protocol.EventPayload{
		protocol.QueuePayload{Items: queue},
		protocol.SnapshotPayload{Lifecycle: protocol.LifecycleReady, Queue: queue, ContextState: protocol.ContextPending},
	} {
		encoded, err := protocol.EncodeEvent(validEvent(payload))
		require.NoError(t, err, "%T", payload)
		require.LessOrEqual(t, len(encoded), protocol.MaxEventBytes)
	}
}

func TestQueueEditAllowsEmptyCaptionForBrokerValidationAgainstQueuedImages(t *testing.T) {
	conversationID := idC
	command := protocol.Command{
		APIVersion: protocol.APIVersion, CommandID: idA, ClientID: idB, ConversationID: &conversationID, Type: protocol.CommandQueueEdit,
		Payload: protocol.QueueEditPayload{MessageID: makeID(200), Content: protocol.TextContent("")},
	}
	encoded, err := protocol.EncodeCommand(command)
	require.NoError(t, err)
	_, err = protocol.DecodeCommand(encoded)
	require.NoError(t, err)
}

func TestAcceptedPageBoundariesFitEventFrame(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	timeline := make([]protocol.TimelineItem, protocol.MaxPageSize)
	remaining := protocol.MaxTimelineBytes
	for index := range timeline {
		size := remaining / (len(timeline) - index)
		remaining -= size
		timeline[index] = protocol.TimelineItem{ItemID: makeID(byte(index)), Kind: protocol.TimelineActivity, Text: exactBytes(`"\`, size), CreatedAt: now}
	}

	history := make([]protocol.ArchiveItem, protocol.MaxPageSize)
	for index := range history {
		history[index] = protocol.ArchiveItem{ArchiveID: makeID(byte(index)), CreatedAt: now, UpdatedAt: now, Provider: protocol.ProviderPi, Model: exactBytes(`"\`, protocol.MaxTitleBytes), Preview: exactBytes(`"\`, protocol.MaxTitleBytes)}
	}

	for _, payload := range []protocol.EventPayload{
		protocol.TimelinePayload{CommandID: idA, Items: timeline},
		protocol.HistoryPayload{CommandID: idA, Items: history},
	} {
		encoded, err := protocol.EncodeEvent(validEvent(payload))
		require.NoError(t, err, "%T", payload)
		require.LessOrEqual(t, len(encoded), protocol.MaxEventBytes)
	}
}

func TestApplicationJSONEncodingDoesNotEscapeHTMLAndRejectsDisallowedC0Text(t *testing.T) {
	conversationID := idC
	command := protocol.Command{
		APIVersion: protocol.APIVersion, CommandID: idA, ClientID: idB, ConversationID: &conversationID, Type: protocol.CommandQueueEdit,
		Payload: protocol.QueueEditPayload{MessageID: makeID(200), Content: protocol.TextContent("<>&")},
	}
	encoded, err := protocol.EncodeCommand(command)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"text":"<>&"`)
	require.NotContains(t, string(encoded), `\u003c`)

	event := validEvent(protocol.AssistantDeltaPayload{TurnID: idC, MessageID: idA, Text: "<>&"})
	encoded, err = protocol.EncodeEvent(event)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"text":"<>&"`)
	require.NotContains(t, string(encoded), `\u003c`)

	for control := byte(0); control < 0x20; control++ {
		if control == '\t' || control == '\n' || control == '\r' {
			continue
		}
		bad := command
		bad.Payload = protocol.QueueEditPayload{MessageID: makeID(200), Content: protocol.TextContent("visible" + string(control))}
		_, err = json.Marshal(bad)
		require.ErrorIs(t, err, protocol.ErrInvalidMessage, "control %#x", control)
	}
}

func TestPageContextDigestAndHTTPSHostnameAreValidated(t *testing.T) {
	valid := validSubmitJSON("initial")
	for name, input := range map[string]string{
		"digest mismatch":     strings.Replace(valid, "2955a3d16e16b3a1044e95e84aea4cd29b37440217bdaff323fb32e31b47159b", digest, 1),
		"empty hostname":      strings.Replace(valid, "https://whiteboard.example/whiteboards/markdown/CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC", "https://:443/path", 1),
		"credentials":         strings.Replace(valid, "https://whiteboard.example/whiteboards/markdown/CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC", "https://user:pass@whiteboard.example/path", 1),
		"malformed authority": strings.Replace(valid, "https://whiteboard.example/whiteboards/markdown/CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC", "https://[::1/path", 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := protocol.DecodeCommand([]byte(input))
			require.ErrorIs(t, err, protocol.ErrInvalidMessage)
		})
	}
}

func TestQueueReplayAndPageBounds(t *testing.T) {
	queue := make([]protocol.QueueItem, protocol.MaxQueueItems+1)
	for index := range queue {
		queue[index] = protocol.QueueItem{TurnID: idB, MessageID: idA, Content: protocol.TextContent("message")}
	}
	err := protocol.ValidateQueue(queue)
	require.ErrorIs(t, err, protocol.ErrMessageTooLarge)

	queue = []protocol.QueueItem{{TurnID: idB, MessageID: idA, Content: protocol.TextContent(strings.Repeat("x", protocol.MaxQueueBytes+1))}}
	err = protocol.ValidateQueue(queue)
	require.ErrorIs(t, err, protocol.ErrMessageTooLarge)

	require.Equal(t, protocol.DefaultPageSize, protocol.NormalizePageSize(0))
	require.Equal(t, protocol.MaxPageSize, protocol.NormalizePageSize(protocol.MaxPageSize))
	require.Equal(t, 0, protocol.NormalizePageSize(protocol.MaxPageSize+1))

	replay := make([]protocol.Event, protocol.MaxReplayEvents+1)
	err = protocol.ValidateReplay(replay)
	require.ErrorIs(t, err, protocol.ErrMessageTooLarge)
}

func TestEventEnvelopeAndEveryPayloadTypeRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	turnID := idC
	payloads := []protocol.EventPayload{
		protocol.SnapshotPayload{Lifecycle: protocol.LifecycleReady, Queue: []protocol.QueueItem{}, ContextState: protocol.ContextPending, ActiveTurnID: nil},
		protocol.CommandResultPayload{CommandID: idA, Status: protocol.CommandSucceeded},
		protocol.TimelinePayload{CommandID: idA, Items: []protocol.TimelineItem{}, NextCursor: nil},
		protocol.HistoryPayload{CommandID: idA, Items: []protocol.ArchiveItem{}, NextCursor: nil},
		protocol.UserMessagePayload{TurnID: idC, MessageID: idA, Content: protocol.TextContent("hello"), CreatedAt: now},
		protocol.AssistantDeltaPayload{TurnID: idC, MessageID: idA, Text: "part"},
		protocol.AssistantMessagePayload{TurnID: idC, MessageID: idA, Text: "answer", CreatedAt: now},
		protocol.QueuePayload{Items: []protocol.QueueItem{}},
		protocol.LifecyclePayload{State: protocol.LifecycleResponding, TurnID: &turnID},
		protocol.ProviderPayload{Provider: protocol.ProviderPi, State: protocol.ProviderReady, Model: "configured-default"},
		protocol.ContextPayload{Digest: digest, State: protocol.ContextAccepted},
		protocol.ActivityPayload{Kind: protocol.ActivityCompaction, Summary: "Conversation compacted."},
		protocol.NewBlockedPayload(protocol.BlockedTool),
		protocol.ErrorPayload{Error: protocol.NewBrowserError(protocol.ErrorProviderCrashed)},
		protocol.CompletionPayload{TurnID: idA},
		protocol.InterruptionPayload{TurnID: idA, Reason: protocol.InterruptionRequested},
		protocol.ArchivePayload{Action: protocol.ArchiveRestored, ArchiveID: idA},
	}
	for _, payload := range payloads {
		event := protocol.Event{APIVersion: protocol.APIVersion, EventID: idA, ConversationID: idB, Type: payload.EventType(), Timestamp: now, Payload: payload}
		encoded, err := protocol.EncodeEvent(event)
		require.NoError(t, err, "%T", payload)
		decoded, err := protocol.DecodeEvent(encoded)
		require.NoError(t, err, "%T: %s", payload, encoded)
		require.Equal(t, event.Type, decoded.Type)
		require.NotNil(t, decoded.Payload)
	}
}

func TestEventStrictnessSizeAndSafeSerialization(t *testing.T) {
	event := protocol.Event{APIVersion: protocol.APIVersion, EventID: idA, ConversationID: idB, Type: protocol.EventError, Timestamp: time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC), Payload: protocol.ErrorPayload{Error: protocol.NewBrowserError(protocol.ErrorProviderCrashed)}}
	encoded, err := protocol.EncodeEvent(event)
	require.NoError(t, err)
	serialized := string(encoded)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(encoded, &wire))
	assertNoForbiddenWireKeys(t, wire)

	_, err = protocol.DecodeEvent(append(encoded, []byte(` {}`)...))
	require.ErrorIs(t, err, protocol.ErrInvalidMessage)
	duplicate := strings.Replace(serialized, `"event_id":`, `"event_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","event_id":`, 1)
	_, err = protocol.DecodeEvent([]byte(duplicate))
	require.ErrorIs(t, err, protocol.ErrInvalidMessage)

	large := event
	large.Type = protocol.EventAssistantMessage
	large.Payload = protocol.AssistantMessagePayload{TurnID: idC, MessageID: idA, Text: strings.Repeat("x", protocol.MaxEventBytes), CreatedAt: event.Timestamp}
	_, err = protocol.EncodeEvent(large)
	require.ErrorIs(t, err, protocol.ErrMessageTooLarge)
}

func TestSplitDeltaUsesUTF8BoundariesAndByteLimit(t *testing.T) {
	input := strings.Repeat("🙂", protocol.MaxDeltaBytes/4+2)
	parts, err := protocol.SplitDelta(input)
	require.NoError(t, err)
	require.Equal(t, input, strings.Join(parts, ""))
	require.Len(t, parts, 2)
	for _, part := range parts {
		require.LessOrEqual(t, len([]byte(part)), protocol.MaxDeltaBytes)
		require.True(t, strings.ToValidUTF8(part, "") == part)
	}
	_, err = protocol.SplitDelta(string([]byte{0xff}))
	require.ErrorIs(t, err, protocol.ErrInvalidMessage)
}

func TestBrowserErrorsHaveOnlyFixedBrokerOwnedRepresentations(t *testing.T) {
	for _, code := range protocol.AllBrowserErrorCodes() {
		err := protocol.NewBrowserError(code)
		require.Equal(t, code, err.Code())
		require.NotEmpty(t, err.Message())
		require.NotEmpty(t, err.Action())
		encoded, marshalErr := json.Marshal(err)
		require.NoError(t, marshalErr)
		require.NotContains(t, string(encoded), "%!")
	}

	var decoded protocol.BrowserError
	err := json.Unmarshal([]byte(`{"code":"provider_crashed","message":"/secret/path","action":"retry"}`), &decoded)
	require.Error(t, err)
}

func TestProviderErrorsRemainProviderNeutral(t *testing.T) {
	for _, code := range []protocol.BrowserErrorCode{
		protocol.ErrorProviderMissing,
		protocol.ErrorAuthenticationRequired,
		protocol.ErrorNoUsableModel,
		protocol.ErrorProviderStartupFailed,
		protocol.ErrorContentOnlyUnavailable,
		protocol.ErrorProviderCrashed,
		protocol.ErrorProviderRecoveryFailed,
	} {
		message := protocol.NewBrowserError(code).Message()
		require.NotContains(t, message, "Pi")
		require.NotContains(t, message, "Codex")
	}
}

func TestActiveTurnIdentityIsExplicitAcrossSubmissionQueueAndEvents(t *testing.T) {
	decoded, err := protocol.DecodeCommand([]byte(validSubmitJSON("initial")))
	require.NoError(t, err)
	require.Equal(t, "EEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE", decoded.Payload.(protocol.SubmitPayload).TurnID)

	active := idC
	event := validEvent(protocol.SnapshotPayload{
		Lifecycle: protocol.LifecycleResponding, Queue: []protocol.QueueItem{{TurnID: idA, MessageID: idB, Content: protocol.TextContent("next")}}, ContextState: protocol.ContextAccepted, ActiveTurnID: &active,
	})
	encoded, err := protocol.EncodeEvent(event)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"active_turn_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"`)
	require.Contains(t, string(encoded), `"turn_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"`)
}

func TestDefaultContextWorstCaseJSONEscapingFitsTransport(t *testing.T) {
	require.Equal(t, 67<<20, protocol.MaxContextCommandBytes)
	conversationID := idC
	markdown := strings.Repeat("\x01", protocol.MaxMarkdownBytes)
	creatorContext := strings.Repeat("\x02", protocol.MaxCreatorContextBytes)
	command := protocol.Command{
		APIVersion: protocol.APIVersion, CommandID: idA, ClientID: idB, ConversationID: &conversationID, Type: protocol.CommandSubmit,
		Payload: protocol.SubmitPayload{TurnID: "EEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE", MessageID: "DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD", Content: protocol.TextContent(strings.Repeat("\t", protocol.MaxMessageBytes)), Context: &protocol.PageContext{
			Revision: protocol.ContextInitial, Markdown: markdown, CreatorContext: creatorContext, Title: strings.Repeat(`"`, protocol.MaxTitleBytes), URL: "https://whiteboard.example/?" + strings.Repeat("&", protocol.MaxURLBytes-len("https://whiteboard.example/?")),
			Resource: protocol.Resource{Kind: protocol.ResourceMarkdown, ID: idC, CreatedAt: time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC), UpdatedAt: time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)}, Digest: agent.CalculateContextDigest([]byte(markdown), []byte(creatorContext)),
		}},
	}
	encoded, err := protocol.EncodeCommand(command)
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), protocol.MaxContextCommandBytes)
	_, err = protocol.DecodeCommand(encoded)
	require.NoError(t, err)
}

func TestPageEventsAreCorrelatedAndHaveStableCursors(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	next := idB
	payload := protocol.TimelinePayload{CommandID: idA, Items: []protocol.TimelineItem{{ItemID: idB, Kind: protocol.TimelineActivity, Text: "Compacted.", CreatedAt: now}}, NextCursor: &next}
	encoded, err := protocol.EncodeEvent(validEvent(payload))
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"`)
	require.Contains(t, string(encoded), `"item_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"`)
	require.Contains(t, string(encoded), `"next_cursor":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"`)

	bad := payload
	invalidCursor := "bad"
	bad.NextCursor = &invalidCursor
	_, err = protocol.EncodeEvent(validEvent(bad))
	require.ErrorIs(t, err, protocol.ErrInvalidMessage)
}

func TestEventRecursiveStrictness(t *testing.T) {
	encoded, err := protocol.EncodeEvent(validEvent(protocol.UserMessagePayload{TurnID: idC, MessageID: idA, Content: protocol.TextContent("hello"), CreatedAt: time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)}))
	require.NoError(t, err)
	valid := string(encoded)
	tests := map[string]string{
		"nested duplicate": strings.Replace(valid, `"turn_id":`, `"turn_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","turn_id":`, 1),
		"nested unknown":   strings.Replace(valid, `"text":`, `"native":{},"text":`, 1),
		"nested null":      strings.Replace(valid, `"text":"hello"`, `"text":null`, 1),
		"trailing":         valid + `{}`,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			_, decodeErr := protocol.DecodeEvent([]byte(input))
			require.ErrorIs(t, decodeErr, protocol.ErrInvalidMessage)
		})
	}
}

func TestEventRequiredFieldsAndTypedNilAreRejected(t *testing.T) {
	encoded, err := protocol.EncodeEvent(validEvent(protocol.UserMessagePayload{TurnID: idC, MessageID: idA, Content: protocol.TextContent("hello"), CreatedAt: time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)}))
	require.NoError(t, err)
	missingTurn := strings.Replace(string(encoded), `"turn_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC",`, "", 1)
	_, err = protocol.DecodeEvent([]byte(missingTurn))
	require.ErrorIs(t, err, protocol.ErrInvalidMessage)

	var payload *protocol.SnapshotPayload
	require.NotPanics(t, func() {
		_, err = protocol.EncodeEvent(protocol.Event{APIVersion: protocol.APIVersion, EventID: idA, ConversationID: idB, Type: protocol.EventSnapshot, Timestamp: time.Now().UTC(), Payload: payload})
	})
	require.ErrorIs(t, err, protocol.ErrInvalidMessage)
}

func TestNewEventContractFieldsAreRequired(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	active := idC
	next := idB
	archiveNext := idC
	cases := []struct {
		name    string
		payload protocol.EventPayload
		field   string
	}{
		{"snapshot active turn", protocol.SnapshotPayload{Lifecycle: protocol.LifecycleResponding, Queue: []protocol.QueueItem{}, ContextState: protocol.ContextAccepted, ActiveTurnID: &active}, `,"active_turn_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"`},
		{"queue item turn", protocol.QueuePayload{Items: []protocol.QueueItem{{TurnID: idC, MessageID: idA, Content: protocol.TextContent("queued")}}}, `"turn_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC",`},
		{"timeline command", protocol.TimelinePayload{CommandID: idA, Items: []protocol.TimelineItem{{ItemID: idB, Kind: protocol.TimelineActivity, Text: "status", CreatedAt: now}}, NextCursor: &next}, `"command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",`},
		{"timeline item", protocol.TimelinePayload{CommandID: idA, Items: []protocol.TimelineItem{{ItemID: idB, Kind: protocol.TimelineActivity, Text: "status", CreatedAt: now}}, NextCursor: &next}, `"item_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",`},
		{"timeline cursor", protocol.TimelinePayload{CommandID: idA, Items: []protocol.TimelineItem{{ItemID: idB, Kind: protocol.TimelineActivity, Text: "status", CreatedAt: now}}, NextCursor: &next}, `,"next_cursor":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"`},
		{"history command", protocol.HistoryPayload{CommandID: idA, Items: []protocol.ArchiveItem{{ArchiveID: idC, CreatedAt: now, UpdatedAt: now, Provider: protocol.ProviderPi}}, NextCursor: &archiveNext}, `"command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",`},
		{"history cursor", protocol.HistoryPayload{CommandID: idA, Items: []protocol.ArchiveItem{{ArchiveID: idC, CreatedAt: now, UpdatedAt: now, Provider: protocol.ProviderPi}}, NextCursor: &archiveNext}, `,"next_cursor":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"`},
		{"lifecycle turn", protocol.LifecyclePayload{State: protocol.LifecycleResponding, TurnID: &active}, `,"turn_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := protocol.EncodeEvent(validEvent(test.payload))
			require.NoError(t, err)
			without := strings.Replace(string(encoded), test.field, "", 1)
			require.NotEqual(t, string(encoded), without)
			_, err = protocol.DecodeEvent([]byte(without))
			require.ErrorIs(t, err, protocol.ErrInvalidMessage)
		})
	}

	withoutTurn := strings.Replace(validSubmitJSON("initial"), `"turn_id":"EEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE",`, "", 1)
	_, err := protocol.DecodeCommand([]byte(withoutTurn))
	require.ErrorIs(t, err, protocol.ErrInvalidMessage)
}

func TestNormalizedEventAndSummaryBoundsAreMeasuredInBytes(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	_, err := protocol.EncodeEvent(validEvent(protocol.ActivityPayload{Kind: protocol.ActivityStatus, Summary: strings.Repeat("é", protocol.MaxSummaryBytes/2+1)}))
	require.ErrorIs(t, err, protocol.ErrInvalidMessage)
	_, err = protocol.EncodeEvent(validEvent(protocol.UserMessagePayload{TurnID: idC, MessageID: idA, Content: protocol.TextContent(string([]byte{0xff})), CreatedAt: now}))
	require.ErrorIs(t, err, protocol.ErrInvalidMessage)
}

func TestProviderReadyEventRequiresResolvedModel(t *testing.T) {
	_, err := protocol.EncodeEvent(validEvent(protocol.ProviderPayload{Provider: protocol.ProviderPi, State: protocol.ProviderReady}))
	require.ErrorIs(t, err, protocol.ErrInvalidMessage)
}

func TestBrowserErrorTaxonomyCoversBrokerAndProviderOutcomes(t *testing.T) {
	required := []protocol.BrowserErrorCode{
		protocol.ErrorInvalidCommand, protocol.ErrorInvalidState, protocol.ErrorQueueFull, protocol.ErrorActiveTurnConflict,
		protocol.ErrorStaleReference, protocol.ErrorReplayWindowUnavailable, protocol.ErrorStateRepairFailed,
		protocol.ErrorArchiveDeleteRetained, protocol.ErrorBrokerShuttingDown, protocol.ErrorProviderProtocolFailure,
		protocol.ErrorProviderMalformedStream, protocol.ErrorAcceptanceOutcomeUnknown,
	}
	for _, code := range required {
		require.Contains(t, protocol.AllBrowserErrorCodes(), code)
		err := protocol.NewBrowserError(code)
		require.NotEmpty(t, err.Message())
		require.NotEmpty(t, err.Action())
	}
	require.Equal(t, protocol.ActionNone, protocol.NewBrowserError(protocol.ErrorInvalidCommand).Action())
}

func validEvent(payload protocol.EventPayload) protocol.Event {
	return protocol.Event{APIVersion: protocol.APIVersion, EventID: idA, ConversationID: idB, Type: payload.EventType(), Timestamp: time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC), Payload: payload}
}

func assertNoForbiddenWireKeys(t *testing.T, value any) {
	t.Helper()
	forbidden := map[string]bool{"native_session": true, "raw": true, "path": true, "credential": true, "reasoning": true}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			require.False(t, forbidden[key], "forbidden wire key %q", key)
			assertNoForbiddenWireKeys(t, child)
		}
	case []any:
		for _, child := range typed {
			assertNoForbiddenWireKeys(t, child)
		}
	}
}

func validSubmitJSON(revision string) string {
	return `{"api_version":"3","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"submit","payload":{"turn_id":"EEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE","message_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD","content":{"parts":[{"type":"text","text":"question"}]},"context":{"revision":"` + revision + `","markdown":"# page","creator_context":"creator summary","title":"Page title","url":"https://whiteboard.example/whiteboards/markdown/CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","resource":{"kind":"markdown","id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","created_at":"2026-07-27T01:02:03Z","updated_at":"2026-07-27T02:03:04Z","expires_at":null},"digest":"2955a3d16e16b3a1044e95e84aea4cd29b37440217bdaff323fb32e31b47159b"}}}`
}

func exactBytes(character string, size int) string {
	return strings.Repeat(character, size/len(character)) + strings.Repeat("x", size%len(character))
}

func makeID(seed byte) string {
	raw := bytes.Repeat([]byte{seed}, 24)
	return base64.RawURLEncoding.EncodeToString(raw)
}
