package agentprotocol_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/stretchr/testify/require"
)

const (
	idA    = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	idB    = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	idC    = "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
	digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func TestDecodeConnectCommandContract(t *testing.T) {
	input := `{"api_version":"1","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":null,"type":"connect","payload":{"provider":"pi","resource":{"kind":"markdown","id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","created_at":"2026-07-27T01:02:03Z","updated_at":"2026-07-27T02:03:04Z","expires_at":null},"context_digest":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","replay_after":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"}}`

	command, err := agentprotocol.DecodeCommand([]byte(input))
	require.NoError(t, err)
	require.Equal(t, agentprotocol.CommandConnect, command.Type)
	require.Nil(t, command.ConversationID)
	payload, ok := command.Payload.(agentprotocol.ConnectPayload)
	require.True(t, ok)
	require.Equal(t, agentprotocol.ProviderPi, payload.Provider)
	require.Equal(t, agentprotocol.ResourceMarkdown, payload.Resource.Kind)
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

func TestDecodeSubmitCarriesCompleteInitialOrReplacementContext(t *testing.T) {
	for _, revision := range []agentprotocol.ContextRevision{agentprotocol.ContextInitial, agentprotocol.ContextReplacement} {
		t.Run(string(revision), func(t *testing.T) {
			command := validSubmitJSON(string(revision))
			decoded, err := agentprotocol.DecodeCommand([]byte(command))
			require.NoError(t, err)
			payload := decoded.Payload.(agentprotocol.SubmitPayload)
			require.Equal(t, "question", payload.Message)
			require.NotNil(t, payload.Context)
			require.Equal(t, "# page", payload.Context.Markdown)
			require.Equal(t, "creator summary", payload.Context.CreatorContext)
			require.Equal(t, revision, payload.Context.Revision)
		})
	}

	withoutContext := `{"api_version":"1","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"submit","payload":{"turn_id":"EEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE","message_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD","message":"follow up"}}`
	decoded, err := agentprotocol.DecodeCommand([]byte(withoutContext))
	require.NoError(t, err)
	payload := decoded.Payload.(agentprotocol.SubmitPayload)
	require.Equal(t, "EEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE", payload.TurnID)
	require.Nil(t, payload.Context)
}

func TestAllCommandPayloadsAreClosedAndValidated(t *testing.T) {
	commands := []string{
		`{"api_version":"1","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"queue_edit","payload":{"message_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD","message":"edited"}}`,
		`{"api_version":"1","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"queue_remove","payload":{"message_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"}}`,
		`{"api_version":"1","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"interrupt","payload":{"turn_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"}}`,
		`{"api_version":"1","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"retry","payload":{"turn_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"}}`,
		`{"api_version":"1","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"new","payload":{}}`,
		`{"api_version":"1","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"archive_list","payload":{"limit":50}}`,
		`{"api_version":"1","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"archive_restore","payload":{"archive_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"}}`,
		`{"api_version":"1","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"archive_delete","payload":{"archive_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"}}`,
		`{"api_version":"1","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"history_page","payload":{"before":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD","limit":100}}`,
		`{"api_version":"1","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"resync","payload":{"after_event_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"}}`,
	}
	for _, input := range commands {
		decoded, err := agentprotocol.DecodeCommand([]byte(input))
		require.NoError(t, err, input)
		require.NotNil(t, decoded.Payload)
	}
}

func TestCommandStrictDecoding(t *testing.T) {
	valid := `{"api_version":"1","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"queue_remove","payload":{"message_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"}}`
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
		"bad version":                strings.Replace(valid, `"api_version":"1"`, `"api_version":"2"`, 1),
		"bad id":                     strings.Replace(valid, idA, "short", 1),
		"bad enum":                   strings.Replace(valid, `"type":"queue_remove"`, `"type":"native_raw"`, 1),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := agentprotocol.DecodeCommand([]byte(input))
			require.ErrorIs(t, err, agentprotocol.ErrInvalidMessage)
		})
	}

	invalidUTF8 := append([]byte(valid[:len(valid)-1]), 0xff, '}')
	_, err := agentprotocol.DecodeCommand(invalidUTF8)
	require.ErrorIs(t, err, agentprotocol.ErrInvalidMessage)
}

func TestConnectRejectsMalformedNestedSecurityValues(t *testing.T) {
	valid := `{"api_version":"1","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":null,"type":"connect","payload":{"provider":"pi","resource":{"kind":"markdown","id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","created_at":"2026-07-27T01:02:03Z","updated_at":"2026-07-27T02:03:04Z","expires_at":null},"context_digest":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}}`
	tests := map[string]string{
		"nested duplicate":    strings.Replace(valid, `"kind":"markdown"`, `"kind":"markdown","kind":"markdown"`, 1),
		"uppercase digest":    strings.Replace(valid, digest, strings.ToUpper(digest), 1),
		"malformed timestamp": strings.Replace(valid, "2026-07-27T01:02:03Z", "yesterday", 1),
		"wrong resource kind": strings.Replace(valid, `"kind":"markdown"`, `"kind":"html"`, 1),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := agentprotocol.DecodeCommand([]byte(input))
			require.ErrorIs(t, err, agentprotocol.ErrInvalidMessage)
		})
	}

	badURL := strings.Replace(validSubmitJSON("initial"), "https://whiteboard.example/", "file:///", 1)
	_, err := agentprotocol.DecodeCommand([]byte(badURL))
	require.ErrorIs(t, err, agentprotocol.ErrInvalidMessage)
}

func TestCommandBoundsAreMeasuredInBytes(t *testing.T) {
	ordinary := `{"api_version":"1","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"queue_edit","payload":{"message_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD","message":"` + strings.Repeat("é", agentprotocol.MaxOrdinaryCommandBytes) + `"}}`
	_, err := agentprotocol.DecodeCommand([]byte(ordinary))
	require.ErrorIs(t, err, agentprotocol.ErrMessageTooLarge)

	context := validSubmitJSON("initial")
	context = strings.Replace(context, `"# page"`, `"`+strings.Repeat("x", agentprotocol.MaxContextCommandBytes)+`"`, 1)
	_, err = agentprotocol.DecodeCommand([]byte(context))
	require.ErrorIs(t, err, agentprotocol.ErrMessageTooLarge)

	tooLongTitle := strings.Replace(validSubmitJSON("initial"), `"Page title"`, `"`+strings.Repeat("t", agentprotocol.MaxTitleBytes+1)+`"`, 1)
	_, err = agentprotocol.DecodeCommand([]byte(tooLongTitle))
	require.ErrorIs(t, err, agentprotocol.ErrInvalidMessage)

	tooLongURL := strings.Replace(validSubmitJSON("initial"), `https://whiteboard.example/whiteboards/markdown/CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC`, `https://whiteboard.example/`+strings.Repeat("u", agentprotocol.MaxURLBytes), 1)
	_, err = agentprotocol.DecodeCommand([]byte(tooLongURL))
	require.ErrorIs(t, err, agentprotocol.ErrInvalidMessage)
}

func TestQueueReplayAndPageBounds(t *testing.T) {
	queue := make([]agentprotocol.QueueItem, agentprotocol.MaxQueueItems+1)
	for index := range queue {
		queue[index] = agentprotocol.QueueItem{TurnID: idB, MessageID: idA, Message: "message"}
	}
	err := agentprotocol.ValidateQueue(queue)
	require.ErrorIs(t, err, agentprotocol.ErrMessageTooLarge)

	queue = []agentprotocol.QueueItem{{TurnID: idB, MessageID: idA, Message: strings.Repeat("x", agentprotocol.MaxQueueBytes+1)}}
	err = agentprotocol.ValidateQueue(queue)
	require.ErrorIs(t, err, agentprotocol.ErrMessageTooLarge)

	require.Equal(t, agentprotocol.DefaultPageSize, agentprotocol.NormalizePageSize(0))
	require.Equal(t, agentprotocol.MaxPageSize, agentprotocol.NormalizePageSize(agentprotocol.MaxPageSize))
	require.Equal(t, 0, agentprotocol.NormalizePageSize(agentprotocol.MaxPageSize+1))

	replay := make([]agentprotocol.Event, agentprotocol.MaxReplayEvents+1)
	err = agentprotocol.ValidateReplay(replay)
	require.ErrorIs(t, err, agentprotocol.ErrMessageTooLarge)
}

func TestEventEnvelopeAndEveryPayloadTypeRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	turnID := idC
	payloads := []agentprotocol.EventPayload{
		agentprotocol.SnapshotPayload{Lifecycle: agentprotocol.LifecycleReady, Queue: []agentprotocol.QueueItem{}, ContextState: agentprotocol.ContextPending, ActiveTurnID: nil},
		agentprotocol.CommandResultPayload{CommandID: idA, Status: agentprotocol.CommandSucceeded},
		agentprotocol.TimelinePayload{CommandID: idA, Items: []agentprotocol.TimelineItem{}, NextCursor: nil},
		agentprotocol.HistoryPayload{CommandID: idA, Items: []agentprotocol.ArchiveItem{}, NextCursor: nil},
		agentprotocol.UserMessagePayload{TurnID: idC, MessageID: idA, Text: "hello", CreatedAt: now},
		agentprotocol.AssistantDeltaPayload{TurnID: idC, MessageID: idA, Text: "part"},
		agentprotocol.AssistantMessagePayload{TurnID: idC, MessageID: idA, Text: "answer", CreatedAt: now},
		agentprotocol.QueuePayload{Items: []agentprotocol.QueueItem{}},
		agentprotocol.LifecyclePayload{State: agentprotocol.LifecycleResponding, TurnID: &turnID},
		agentprotocol.ProviderPayload{Provider: agentprotocol.ProviderPi, State: agentprotocol.ProviderReady, Model: "configured-default"},
		agentprotocol.ContextPayload{Digest: digest, State: agentprotocol.ContextAccepted},
		agentprotocol.ActivityPayload{Kind: agentprotocol.ActivityCompaction, Summary: "Conversation compacted."},
		agentprotocol.NewBlockedPayload(agentprotocol.BlockedTool),
		agentprotocol.ErrorPayload{Error: agentprotocol.NewBrowserError(agentprotocol.ErrorProviderCrashed)},
		agentprotocol.CompletionPayload{TurnID: idA},
		agentprotocol.InterruptionPayload{TurnID: idA, Reason: agentprotocol.InterruptionRequested},
		agentprotocol.ArchivePayload{Action: agentprotocol.ArchiveRestored, ArchiveID: idA},
	}
	for _, payload := range payloads {
		event := agentprotocol.Event{APIVersion: agentprotocol.APIVersion, EventID: idA, ConversationID: idB, Type: payload.EventType(), Timestamp: now, Payload: payload}
		encoded, err := agentprotocol.EncodeEvent(event)
		require.NoError(t, err, "%T", payload)
		decoded, err := agentprotocol.DecodeEvent(encoded)
		require.NoError(t, err, "%T: %s", payload, encoded)
		require.Equal(t, event.Type, decoded.Type)
		require.NotNil(t, decoded.Payload)
	}
}

func TestEventStrictnessSizeAndSafeSerialization(t *testing.T) {
	event := agentprotocol.Event{APIVersion: agentprotocol.APIVersion, EventID: idA, ConversationID: idB, Type: agentprotocol.EventError, Timestamp: time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC), Payload: agentprotocol.ErrorPayload{Error: agentprotocol.NewBrowserError(agentprotocol.ErrorProviderCrashed)}}
	encoded, err := agentprotocol.EncodeEvent(event)
	require.NoError(t, err)
	serialized := string(encoded)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(encoded, &wire))
	assertNoForbiddenWireKeys(t, wire)

	_, err = agentprotocol.DecodeEvent(append(encoded, []byte(` {}`)...))
	require.ErrorIs(t, err, agentprotocol.ErrInvalidMessage)
	duplicate := strings.Replace(serialized, `"event_id":`, `"event_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","event_id":`, 1)
	_, err = agentprotocol.DecodeEvent([]byte(duplicate))
	require.ErrorIs(t, err, agentprotocol.ErrInvalidMessage)

	large := event
	large.Type = agentprotocol.EventAssistantMessage
	large.Payload = agentprotocol.AssistantMessagePayload{TurnID: idC, MessageID: idA, Text: strings.Repeat("x", agentprotocol.MaxEventBytes), CreatedAt: event.Timestamp}
	_, err = agentprotocol.EncodeEvent(large)
	require.ErrorIs(t, err, agentprotocol.ErrMessageTooLarge)
}

func TestSplitDeltaUsesUTF8BoundariesAndByteLimit(t *testing.T) {
	input := strings.Repeat("🙂", agentprotocol.MaxDeltaBytes/4+2)
	parts, err := agentprotocol.SplitDelta(input)
	require.NoError(t, err)
	require.Equal(t, input, strings.Join(parts, ""))
	require.Len(t, parts, 2)
	for _, part := range parts {
		require.LessOrEqual(t, len([]byte(part)), agentprotocol.MaxDeltaBytes)
		require.True(t, strings.ToValidUTF8(part, "") == part)
	}
	_, err = agentprotocol.SplitDelta(string([]byte{0xff}))
	require.ErrorIs(t, err, agentprotocol.ErrInvalidMessage)
}

func TestBrowserErrorsHaveOnlyFixedBrokerOwnedRepresentations(t *testing.T) {
	for _, code := range agentprotocol.AllBrowserErrorCodes() {
		err := agentprotocol.NewBrowserError(code)
		require.Equal(t, code, err.Code())
		require.NotEmpty(t, err.Message())
		require.NotEmpty(t, err.Action())
		encoded, marshalErr := json.Marshal(err)
		require.NoError(t, marshalErr)
		require.NotContains(t, string(encoded), "%!")
	}

	var decoded agentprotocol.BrowserError
	err := json.Unmarshal([]byte(`{"code":"provider_crashed","message":"/secret/path","action":"retry"}`), &decoded)
	require.Error(t, err)
}

func TestActiveTurnIdentityIsExplicitAcrossSubmissionQueueAndEvents(t *testing.T) {
	decoded, err := agentprotocol.DecodeCommand([]byte(validSubmitJSON("initial")))
	require.NoError(t, err)
	require.Equal(t, "EEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE", decoded.Payload.(agentprotocol.SubmitPayload).TurnID)

	active := idC
	event := validEvent(agentprotocol.SnapshotPayload{
		Lifecycle: agentprotocol.LifecycleResponding, Queue: []agentprotocol.QueueItem{{TurnID: idA, MessageID: idB, Message: "next"}}, ContextState: agentprotocol.ContextAccepted, ActiveTurnID: &active,
	})
	encoded, err := agentprotocol.EncodeEvent(event)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"active_turn_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"`)
	require.Contains(t, string(encoded), `"turn_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"`)
}

func TestDefaultContextWorstCaseJSONEscapingFitsTransport(t *testing.T) {
	require.Equal(t, 67<<20, agentprotocol.MaxContextCommandBytes)
	conversationID := idC
	command := agentprotocol.Command{
		APIVersion: agentprotocol.APIVersion, CommandID: idA, ClientID: idB, ConversationID: &conversationID, Type: agentprotocol.CommandSubmit,
		Payload: agentprotocol.SubmitPayload{TurnID: "EEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE", MessageID: "DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD", Message: strings.Repeat("<", agentprotocol.MaxOrdinaryCommandBytes), Context: &agentprotocol.PageContext{
			Revision: agentprotocol.ContextInitial, Markdown: strings.Repeat("<", agentprotocol.MaxMarkdownBytes), CreatorContext: strings.Repeat("&", agentprotocol.MaxCreatorContextBytes), Title: strings.Repeat("<", agentprotocol.MaxTitleBytes), URL: "https://whiteboard.example/?" + strings.Repeat("&", agentprotocol.MaxURLBytes-len("https://whiteboard.example/?")),
			Resource: agentprotocol.Resource{Kind: agentprotocol.ResourceMarkdown, ID: idC, CreatedAt: time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC), UpdatedAt: time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)}, Digest: digest,
		}},
	}
	encoded, err := json.Marshal(command)
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), agentprotocol.MaxContextCommandBytes)
	_, err = agentprotocol.DecodeCommand(encoded)
	require.NoError(t, err)
}

func TestPageEventsAreCorrelatedAndHaveStableCursors(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	next := idB
	payload := agentprotocol.TimelinePayload{CommandID: idA, Items: []agentprotocol.TimelineItem{{ItemID: idB, Kind: agentprotocol.TimelineActivity, Text: "Compacted.", CreatedAt: now}}, NextCursor: &next}
	encoded, err := agentprotocol.EncodeEvent(validEvent(payload))
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"`)
	require.Contains(t, string(encoded), `"item_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"`)
	require.Contains(t, string(encoded), `"next_cursor":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"`)

	bad := payload
	invalidCursor := "bad"
	bad.NextCursor = &invalidCursor
	_, err = agentprotocol.EncodeEvent(validEvent(bad))
	require.ErrorIs(t, err, agentprotocol.ErrInvalidMessage)
}

func TestEventRecursiveStrictness(t *testing.T) {
	encoded, err := agentprotocol.EncodeEvent(validEvent(agentprotocol.UserMessagePayload{TurnID: idC, MessageID: idA, Text: "hello", CreatedAt: time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)}))
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
			_, decodeErr := agentprotocol.DecodeEvent([]byte(input))
			require.ErrorIs(t, decodeErr, agentprotocol.ErrInvalidMessage)
		})
	}
}

func TestEventRequiredFieldsAndTypedNilAreRejected(t *testing.T) {
	encoded, err := agentprotocol.EncodeEvent(validEvent(agentprotocol.UserMessagePayload{TurnID: idC, MessageID: idA, Text: "hello", CreatedAt: time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)}))
	require.NoError(t, err)
	missingTurn := strings.Replace(string(encoded), `"turn_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC",`, "", 1)
	_, err = agentprotocol.DecodeEvent([]byte(missingTurn))
	require.ErrorIs(t, err, agentprotocol.ErrInvalidMessage)

	var payload *agentprotocol.SnapshotPayload
	require.NotPanics(t, func() {
		_, err = agentprotocol.EncodeEvent(agentprotocol.Event{APIVersion: agentprotocol.APIVersion, EventID: idA, ConversationID: idB, Type: agentprotocol.EventSnapshot, Timestamp: time.Now().UTC(), Payload: payload})
	})
	require.ErrorIs(t, err, agentprotocol.ErrInvalidMessage)
}

func TestNewEventContractFieldsAreRequired(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	active := idC
	next := idB
	archiveNext := idC
	cases := []struct {
		name    string
		payload agentprotocol.EventPayload
		field   string
	}{
		{"snapshot active turn", agentprotocol.SnapshotPayload{Lifecycle: agentprotocol.LifecycleResponding, Queue: []agentprotocol.QueueItem{}, ContextState: agentprotocol.ContextAccepted, ActiveTurnID: &active}, `,"active_turn_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"`},
		{"queue item turn", agentprotocol.QueuePayload{Items: []agentprotocol.QueueItem{{TurnID: idC, MessageID: idA, Message: "queued"}}}, `"turn_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC",`},
		{"timeline command", agentprotocol.TimelinePayload{CommandID: idA, Items: []agentprotocol.TimelineItem{{ItemID: idB, Kind: agentprotocol.TimelineActivity, Text: "status", CreatedAt: now}}, NextCursor: &next}, `"command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",`},
		{"timeline item", agentprotocol.TimelinePayload{CommandID: idA, Items: []agentprotocol.TimelineItem{{ItemID: idB, Kind: agentprotocol.TimelineActivity, Text: "status", CreatedAt: now}}, NextCursor: &next}, `"item_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",`},
		{"timeline cursor", agentprotocol.TimelinePayload{CommandID: idA, Items: []agentprotocol.TimelineItem{{ItemID: idB, Kind: agentprotocol.TimelineActivity, Text: "status", CreatedAt: now}}, NextCursor: &next}, `,"next_cursor":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"`},
		{"history command", agentprotocol.HistoryPayload{CommandID: idA, Items: []agentprotocol.ArchiveItem{{ArchiveID: idC, CreatedAt: now, UpdatedAt: now, Provider: agentprotocol.ProviderPi}}, NextCursor: &archiveNext}, `"command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",`},
		{"history cursor", agentprotocol.HistoryPayload{CommandID: idA, Items: []agentprotocol.ArchiveItem{{ArchiveID: idC, CreatedAt: now, UpdatedAt: now, Provider: agentprotocol.ProviderPi}}, NextCursor: &archiveNext}, `,"next_cursor":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"`},
		{"lifecycle turn", agentprotocol.LifecyclePayload{State: agentprotocol.LifecycleResponding, TurnID: &active}, `,"turn_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := agentprotocol.EncodeEvent(validEvent(test.payload))
			require.NoError(t, err)
			without := strings.Replace(string(encoded), test.field, "", 1)
			require.NotEqual(t, string(encoded), without)
			_, err = agentprotocol.DecodeEvent([]byte(without))
			require.ErrorIs(t, err, agentprotocol.ErrInvalidMessage)
		})
	}

	withoutTurn := strings.Replace(validSubmitJSON("initial"), `"turn_id":"EEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE",`, "", 1)
	_, err := agentprotocol.DecodeCommand([]byte(withoutTurn))
	require.ErrorIs(t, err, agentprotocol.ErrInvalidMessage)
}

func TestNormalizedEventAndSummaryBoundsAreMeasuredInBytes(t *testing.T) {
	now := time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC)
	_, err := agentprotocol.EncodeEvent(validEvent(agentprotocol.ActivityPayload{Kind: agentprotocol.ActivityStatus, Summary: strings.Repeat("é", agentprotocol.MaxSummaryBytes/2+1)}))
	require.ErrorIs(t, err, agentprotocol.ErrInvalidMessage)
	_, err = agentprotocol.EncodeEvent(validEvent(agentprotocol.UserMessagePayload{TurnID: idC, MessageID: idA, Text: string([]byte{0xff}), CreatedAt: now}))
	require.ErrorIs(t, err, agentprotocol.ErrInvalidMessage)
}

func TestProviderReadyEventRequiresResolvedModel(t *testing.T) {
	_, err := agentprotocol.EncodeEvent(validEvent(agentprotocol.ProviderPayload{Provider: agentprotocol.ProviderPi, State: agentprotocol.ProviderReady}))
	require.ErrorIs(t, err, agentprotocol.ErrInvalidMessage)
}

func TestBrowserErrorTaxonomyCoversBrokerAndProviderOutcomes(t *testing.T) {
	required := []agentprotocol.BrowserErrorCode{
		agentprotocol.ErrorInvalidCommand, agentprotocol.ErrorInvalidState, agentprotocol.ErrorQueueFull, agentprotocol.ErrorActiveTurnConflict,
		agentprotocol.ErrorStaleReference, agentprotocol.ErrorReplayWindowUnavailable, agentprotocol.ErrorStateRepairFailed,
		agentprotocol.ErrorArchiveDeleteRetained, agentprotocol.ErrorBrokerShuttingDown, agentprotocol.ErrorProviderProtocolFailure,
		agentprotocol.ErrorProviderMalformedStream, agentprotocol.ErrorAcceptanceOutcomeUnknown,
	}
	for _, code := range required {
		require.Contains(t, agentprotocol.AllBrowserErrorCodes(), code)
		err := agentprotocol.NewBrowserError(code)
		require.NotEmpty(t, err.Message())
		require.NotEmpty(t, err.Action())
	}
	require.Equal(t, agentprotocol.ActionNone, agentprotocol.NewBrowserError(agentprotocol.ErrorInvalidCommand).Action())
}

func validEvent(payload agentprotocol.EventPayload) agentprotocol.Event {
	return agentprotocol.Event{APIVersion: agentprotocol.APIVersion, EventID: idA, ConversationID: idB, Type: payload.EventType(), Timestamp: time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC), Payload: payload}
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
	return `{"api_version":"1","command_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","client_id":"BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB","conversation_id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","type":"submit","payload":{"turn_id":"EEEEEEEEEEEEEEEEEEEEEEEEEEEEEEEE","message_id":"DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD","message":"question","context":{"revision":"` + revision + `","markdown":"# page","creator_context":"creator summary","title":"Page title","url":"https://whiteboard.example/whiteboards/markdown/CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","resource":{"kind":"markdown","id":"CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC","created_at":"2026-07-27T01:02:03Z","updated_at":"2026-07-27T02:03:04Z","expires_at":null},"digest":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}}}`
}
