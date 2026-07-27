package broker

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/contextdigest"
	"github.com/edocsss/agent-whiteboard/internal/provider"
	"github.com/stretchr/testify/require"
)

func testID(value byte) string { return strings.Repeat(string(value), 32) }

func sequenceID(value uint64) string {
	var raw [24]byte
	binary.BigEndian.PutUint64(raw[16:], value)
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

func testTime() time.Time { return time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC) }

func testResource(id string) agentprotocol.Resource {
	created := testTime()
	updated := created.Add(time.Minute)
	expires := updated.Add(time.Hour)
	return agentprotocol.Resource{Kind: agentprotocol.ResourceMarkdown, ID: id, CreatedAt: created, UpdatedAt: updated, ExpiresAt: &expires}
}

func testContext(resource agentprotocol.Resource) agentprotocol.PageContext {
	markdown := "# café\n"
	creator := "creator\xE2\x9C\x93"
	return agentprotocol.PageContext{
		Revision:       agentprotocol.ContextInitial,
		Markdown:       markdown,
		CreatorContext: creator,
		Title:          "A title",
		URL:            "https://example.com/board/readme",
		Resource:       resource,
		Digest:         contextdigest.Calculate([]byte(markdown), []byte(creator)),
	}
}

func testProviderContext(resource provider.Resource) provider.PageContext {
	markdown := []byte("# café\n")
	creator := []byte("creator✓")
	return provider.PageContext{
		Revision:       provider.ContextInitial,
		Markdown:       markdown,
		CreatorContext: creator,
		Title:          "A title",
		URL:            "https://example.com/board/readme",
		Resource:       resource,
		Digest:         contextdigest.Calculate(markdown, creator),
	}
}

func TestProviderAndReadinessErrorsAreExhaustivelyMappedWithoutCauses(t *testing.T) {
	expected := map[provider.ProviderErrorCode]agentprotocol.BrowserErrorCode{
		provider.ErrorNotReady:               agentprotocol.ErrorBrokerUnavailable,
		provider.ErrorReadinessFailed:        agentprotocol.ErrorBrokerUnavailable,
		provider.ErrorMissingExecutable:      agentprotocol.ErrorProviderMissing,
		provider.ErrorStartupFailed:          agentprotocol.ErrorProviderStartupFailed,
		provider.ErrorAuthenticationRequired: agentprotocol.ErrorAuthenticationRequired,
		provider.ErrorNoUsableModel:          agentprotocol.ErrorNoUsableModel,
		provider.ErrorContentOnlyUnavailable: agentprotocol.ErrorContentOnlyUnavailable,
		provider.ErrorProtocolIncompatible:   agentprotocol.ErrorProviderProtocolFailure,
		provider.ErrorProtocolFailure:        agentprotocol.ErrorProviderProtocolFailure,
		provider.ErrorMalformedStream:        agentprotocol.ErrorProviderMalformedStream,
		provider.ErrorChildExited:            agentprotocol.ErrorProviderCrashed,
		provider.ErrorNativeSessionMissing:   agentprotocol.ErrorNativeSessionMissing,
		provider.ErrorContextTooLarge:        agentprotocol.ErrorContextTooLarge,
		provider.ErrorAcceptanceUnknown:      agentprotocol.ErrorAcceptanceOutcomeUnknown,
	}
	for _, code := range provider.AllProviderErrorCodes() {
		failure := provider.NewProviderError(code)
		mapped := MapProviderError(failure)
		require.Equal(t, expected[code], mapped.Code())
		require.NotContains(t, mapped.Error(), string(code))
	}
	for _, state := range []provider.ReadinessState{
		provider.MissingExecutable, provider.AuthenticationRequired, provider.StartupFailed,
		provider.NoUsableModel, provider.ContentOnlyUnavailable, provider.ProtocolIncompatible,
	} {
		readiness := provider.Readiness{State: state, Provider: provider.NamePi, Model: "model"}
		mapped, unavailable := MapReadiness(readiness)
		require.True(t, unavailable)
		require.True(t, mapped.Valid())
	}
	mapped, unavailable := MapReadiness(provider.Readiness{State: provider.Ready, Provider: provider.NamePi, Model: "model"})
	require.False(t, unavailable)
	require.False(t, mapped.Valid())
	mapped, unavailable = MapReadiness(provider.Readiness{State: provider.Ready, Provider: provider.Name("other"), Model: "model"})
	require.True(t, unavailable)
	require.Equal(t, agentprotocol.ErrorProviderProtocolFailure, mapped.Code())
	require.Equal(t, agentprotocol.ErrorProviderProtocolFailure, MapProviderError(provider.ProviderError{}).Code())

	cause := errors.New("/private/path native secret")
	mapped = MapError(cause)
	require.Equal(t, agentprotocol.ErrorProviderProtocolFailure, mapped.Code())
	require.NotContains(t, mapped.Error(), cause.Error())
}

func TestConversionsRequireCanonicalAuthorizedOriginAndCopyContextBytes(t *testing.T) {
	resource := testResource(testID('A'))
	identity := ConnectIdentity{Origin: "https://example.com", Provider: agentprotocol.ProviderPi, Resource: resource}
	state, err := ConnectIdentityToState(identity, "https://example.com")
	require.NoError(t, err)
	require.Equal(t, "https://example.com", state.Origin)
	require.NoError(t, state.Validate())

	_, err = ConnectIdentityToState(identity, "https://EXAMPLE.com")
	require.Error(t, err)
	_, err = ConnectIdentityToState(ConnectIdentity{Origin: "https://EXAMPLE.com", Provider: agentprotocol.ProviderPi, Resource: resource}, "https://example.com")
	require.Error(t, err)

	page := testContext(resource)
	converted, err := PageContextToProvider(page, identity, identity.Origin)
	require.NoError(t, err)
	require.Equal(t, page.Digest, converted.Digest)
	require.Equal(t, page.Resource.CreatedAt, converted.Resource.CreatedAt)
	require.Equal(t, page.Resource.UpdatedAt, converted.Resource.UpdatedAt)
	require.Equal(t, *page.Resource.ExpiresAt, *converted.Resource.ExpiresAt)
	back, err := PageContextFromProvider(converted, identity, identity.Origin)
	require.NoError(t, err)
	require.Equal(t, page, back)
	converted.Markdown[0] = 'X'
	converted.CreatorContext[0] = 'X'
	require.Equal(t, byte('#'), page.Markdown[0])
	require.Equal(t, byte('c'), page.CreatorContext[0])

	_, err = PageContextToProvider(page, identity, "https://other.example.com")
	require.Error(t, err)
	page.URL = "http://example.com/board/readme"
	_, err = PageContextToProvider(page, identity, identity.Origin)
	require.Error(t, err)
	page.URL = "https://other.example.com/board/readme"
	_, err = PageContextToProvider(page, identity, identity.Origin)
	require.Error(t, err)
}

func TestResourceConversionsPreserveExactTimestamps(t *testing.T) {
	resource := testResource(testID('B'))
	converted, err := ResourceToProvider(resource)
	require.NoError(t, err)
	require.True(t, converted.CreatedAt == resource.CreatedAt)
	require.True(t, converted.UpdatedAt == resource.UpdatedAt)
	require.True(t, converted.ExpiresAt != resource.ExpiresAt)
	require.True(t, *converted.ExpiresAt == *resource.ExpiresAt)
	convertedBack, err := ResourceFromProvider(converted)
	require.NoError(t, err)
	require.Equal(t, resource, convertedBack)
}

func TestReplayLogUsesWireBytesEvictsExactlyAndFiltersTargets(t *testing.T) {
	log := NewReplayLog()
	conversation := testID('C')
	clientA := testID('D')
	clientB := testID('E')
	now := testTime()
	first := agentprotocol.Event{APIVersion: agentprotocol.APIVersion, EventID: sequenceID(1), ConversationID: conversation, Type: agentprotocol.EventQueue, Timestamp: now, Payload: agentprotocol.QueuePayload{Items: []agentprotocol.QueueItem{{TurnID: testID('F'), MessageID: testID('G'), Message: "✓"}}}}
	encoded, err := agentprotocol.EncodeEvent(first)
	require.NoError(t, err)
	require.NoError(t, log.Append(first))
	require.Equal(t, len(encoded), log.Bytes())

	targeted := first
	targeted.EventID = sequenceID(2)
	require.NoError(t, log.AppendForClient(clientA, targeted))
	broadcast := first
	broadcast.EventID = sequenceID(3)
	require.NoError(t, log.Append(broadcast))

	events, err := log.ReplayForClient(clientB, first.EventID)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, broadcast.EventID, events[0].EventID)
	_, err = log.ReplayForClient(clientB, targeted.EventID)
	require.ErrorIs(t, err, ErrReplayCursorMissing)
	events, err = log.ReplayForClient(clientA, first.EventID)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.NotEqual(t, first.EventID, events[0].EventID) // cursor is strictly later

	// A large escaped payload proves the actual encoded wire size is used.
	for index := 4; index < 1600; index++ {
		event := first
		event.EventID = sequenceID(uint64(index))
		event.Type = agentprotocol.EventActivity
		event.Payload = agentprotocol.ActivityPayload{Kind: agentprotocol.ActivityStatus, Summary: strings.Repeat("✓\\\"", 1200)}
		if err := log.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	require.LessOrEqual(t, log.Len(), MaxReplayEvents)
	require.LessOrEqual(t, log.Bytes(), MaxReplayBytes)
	_, err = log.Replay(first.EventID)
	require.ErrorIs(t, err, ErrReplayCursorEvicted)
}

func TestReplayLogReturnsStablePayloadCopies(t *testing.T) {
	log := NewReplayLog()
	event := agentprotocol.Event{APIVersion: agentprotocol.APIVersion, EventID: sequenceID(100), ConversationID: testID('A'), Type: agentprotocol.EventQueue, Timestamp: testTime(), Payload: agentprotocol.QueuePayload{Items: []agentprotocol.QueueItem{{TurnID: testID('B'), MessageID: testID('C'), Message: "original"}}}}
	require.NoError(t, log.Append(event))
	payload := event.Payload.(agentprotocol.QueuePayload)
	payload.Items[0].Message = "changed"
	got, err := log.Replay("")
	require.NoError(t, err)
	require.Equal(t, "original", got[0].Payload.(agentprotocol.QueuePayload).Items[0].Message)
	require.NotContains(t, string(mustEncodeEvent(t, got[0])), "creator_context")
}

func TestQueueFIFOBoundsDuplicatesEditRemoveAndContextErasure(t *testing.T) {
	queue := NewQueue()
	resource := provider.Resource{Kind: provider.ResourceMarkdown, ID: testID('H'), CreatedAt: testTime(), UpdatedAt: testTime().Add(time.Minute)}
	context := testProviderContext(resource)
	turnA := provider.TurnRequest{TurnID: testID('I'), MessageID: testID('J'), Message: "first", Context: &context}
	turnB := provider.TurnRequest{TurnID: testID('K'), MessageID: testID('L'), Message: "second"}
	require.NoError(t, queue.Enqueue(turnA))
	require.NoError(t, queue.Enqueue(turnB))
	require.Equal(t, len("first")+len("second"), queue.Bytes())
	require.Equal(t, []agentprotocol.QueueItem{{TurnID: turnA.TurnID, MessageID: turnA.MessageID, Message: "first"}, {TurnID: turnB.TurnID, MessageID: turnB.MessageID, Message: "second"}}, queue.Items())
	require.ErrorIs(t, queue.Enqueue(provider.TurnRequest{TurnID: turnA.TurnID, MessageID: testID('M'), Message: "duplicate"}), ErrQueueDuplicateID)
	require.ErrorIs(t, queue.Enqueue(provider.TurnRequest{TurnID: testID('N'), MessageID: turnB.MessageID, Message: "duplicate"}), ErrQueueDuplicateID)
	require.NoError(t, queue.Edit(turnA.MessageID, "edited"))
	require.Equal(t, "edited", queue.Items()[0].Message)
	require.ErrorIs(t, queue.Edit(testID('O'), "missing"), ErrQueueItemNotFound)

	owned := queue.items[0].context
	require.NotNil(t, owned)
	require.NotEmpty(t, owned.Markdown)
	require.NoError(t, queue.Remove(turnA.MessageID))
	require.Nil(t, owned.Markdown)
	require.Nil(t, owned.CreatorContext)
	dequeued, ok := queue.Dequeue()
	require.True(t, ok)
	require.Equal(t, turnB.TurnID, dequeued.TurnID)
	require.True(t, queue.Empty())
}

func TestQueueBoundsAreByteAndItemExact(t *testing.T) {
	queue := NewQueue()
	for index := 0; index < MaxQueueItems; index++ {
		require.NoError(t, queue.Enqueue(provider.TurnRequest{TurnID: sequenceID(uint64(index + 1)), MessageID: sequenceID(uint64(index + 1000)), Message: "x"}))
	}
	require.ErrorIs(t, queue.Enqueue(provider.TurnRequest{TurnID: sequenceID(10000), MessageID: sequenceID(10001), Message: "x"}), ErrQueueFull)

	queue = NewQueue()
	message := strings.Repeat("界", 49152/len("界"))
	require.NoError(t, queue.Enqueue(provider.TurnRequest{TurnID: testID('P'), MessageID: testID('Q'), Message: message}))
	require.NoError(t, queue.Enqueue(provider.TurnRequest{TurnID: testID('R'), MessageID: testID('S'), Message: message}))
	require.ErrorIs(t, queue.Enqueue(provider.TurnRequest{TurnID: testID('W'), MessageID: testID('X'), Message: "x"}), ErrQueueFull)
}

type testIDGenerator struct {
	ids []string
	at  int
}

func (generator *testIDGenerator) NewID() (string, error) {
	id := generator.ids[generator.at]
	generator.at++
	return id, nil
}

type testClock struct{ now time.Time }

func (clock testClock) Now() time.Time { return clock.now }

func TestEventFactoryUsesInjectedIDAndClockAndValidates(t *testing.T) {
	conversation := testID('T')
	ids := &testIDGenerator{ids: []string{testID('U')}}
	clock := testClock{now: testTime()}
	factory, err := NewEventFactory(conversation, ids, clock)
	require.NoError(t, err)
	event, err := factory.New(agentprotocol.CompletionPayload{TurnID: testID('V')})
	require.NoError(t, err)
	require.Equal(t, testID('U'), event.EventID)
	require.Equal(t, conversation, event.ConversationID)
	require.Equal(t, testTime(), event.Timestamp)
	require.Equal(t, agentprotocol.EventCompletion, event.Type)
}

func mustEncodeEvent(t *testing.T, event agentprotocol.Event) []byte {
	encoded, err := agentprotocol.EncodeEvent(event)
	require.NoError(t, err)
	return encoded
}

// Keep the interface assertions close to the deterministic helpers used by
// the factory tests.
var _ common.IDGenerator = (*testIDGenerator)(nil)
var _ common.Clock = testClock{}
