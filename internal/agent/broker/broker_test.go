package broker

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/protocol"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	statepkg "github.com/dndplsidc/agent-whiteboard/internal/agent/state"
	"github.com/dndplsidc/agent-whiteboard/internal/common"
	"github.com/stretchr/testify/require"
)

func testID(value byte) string { return strings.Repeat(string(value), 32) }

func sequenceID(value uint64) string {
	var raw [24]byte
	binary.BigEndian.PutUint64(raw[16:], value)
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

func testTime() time.Time { return time.Date(2026, 7, 27, 1, 2, 3, 0, time.UTC) }

func testResource(id string) protocol.Resource {
	created := testTime()
	updated := created.Add(time.Minute)
	expires := updated.Add(time.Hour)
	return protocol.Resource{Kind: protocol.ResourceMarkdown, ID: id, CreatedAt: created, UpdatedAt: updated, ExpiresAt: &expires}
}

func testContext(resource protocol.Resource) protocol.PageContext {
	source := "# café\n"
	creator := "creator\xE2\x9C\x93"
	digest, _ := agent.CalculateContextDigestForKind(string(resource.Kind), []byte(source), []byte(creator))
	return protocol.PageContext{
		Revision:       protocol.ContextInitial,
		Source:         source,
		CreatorContext: creator,
		Title:          "A title",
		URL:            "https://example.com/board/readme",
		Resource:       resource,
		Digest:         digest,
	}
}

func testProviderContext(resource provider.Resource) provider.PageContext {
	source := []byte("# café\n")
	creator := []byte("creator✓")
	digest, _ := agent.CalculateContextDigestForKind(string(resource.Kind), source, creator)
	return provider.PageContext{
		Revision:       provider.ContextInitial,
		Source:         source,
		CreatorContext: creator,
		Title:          "A title",
		URL:            "https://example.com/board/readme",
		Resource:       resource,
		Digest:         digest,
	}
}

func TestProviderAndReadinessErrorsAreExhaustivelyMappedWithoutCauses(t *testing.T) {
	expected := map[provider.ProviderErrorCode]protocol.BrowserErrorCode{
		provider.ErrorNotReady:                  protocol.ErrorProviderStartupFailed,
		provider.ErrorReadinessFailed:           protocol.ErrorProviderStartupFailed,
		provider.ErrorMissingExecutable:         protocol.ErrorProviderMissing,
		provider.ErrorStartupFailed:             protocol.ErrorProviderStartupFailed,
		provider.ErrorAuthenticationRequired:    protocol.ErrorAuthenticationRequired,
		provider.ErrorNoUsableModel:             protocol.ErrorNoUsableModel,
		provider.ErrorContentOnlyUnavailable:    protocol.ErrorContentOnlyUnavailable,
		provider.ErrorProtocolIncompatible:      protocol.ErrorProviderProtocolFailure,
		provider.ErrorProtocolFailure:           protocol.ErrorProviderProtocolFailure,
		provider.ErrorMalformedStream:           protocol.ErrorProviderMalformedStream,
		provider.ErrorChildExited:               protocol.ErrorProviderCrashed,
		provider.ErrorNativeSessionMissing:      protocol.ErrorNativeSessionMissing,
		provider.ErrorContextTooLarge:           protocol.ErrorContextTooLarge,
		provider.ErrorAcceptanceUnknown:         protocol.ErrorAcceptanceOutcomeUnknown,
		provider.ErrorInvalidModelConfiguration: protocol.ErrorInvalidModelConfiguration,
		provider.ErrorImageInputUnsupported:     protocol.ErrorImageInputUnsupported,
		provider.ErrorImageUnsupported:          protocol.ErrorImageUnsupported,
		provider.ErrorImageTooLarge:             protocol.ErrorImageTooLarge,
		provider.ErrorImageTurnLimit:            protocol.ErrorImageTurnLimit,
		provider.ErrorImageMissing:              protocol.ErrorImageMissing,
		provider.ErrorImageStorageFailure:       protocol.ErrorImageStorageFailure,
		provider.ErrorSkillUnavailable:          protocol.ErrorSkillUnavailable,
		provider.ErrorCompactUnsupported:        protocol.ErrorCompactUnsupported,
		provider.ErrorArchiveDeleteUnsupported:  protocol.ErrorArchiveDeleteUnsupported,
	}
	for _, code := range provider.AllProviderErrorCodes() {
		failure := provider.NewProviderError(code)
		mapped := MapProviderError(failure)
		require.Equal(t, expected[code], mapped.Code())
		require.NotContains(t, mapped.Error(), string(code))
	}
	readinessCodes := map[provider.ReadinessState]protocol.BrowserErrorCode{
		provider.MissingExecutable:      protocol.ErrorProviderMissing,
		provider.AuthenticationRequired: protocol.ErrorAuthenticationRequired,
		provider.StartupFailed:          protocol.ErrorProviderStartupFailed,
		provider.NoUsableModel:          protocol.ErrorNoUsableModel,
		provider.ContentOnlyUnavailable: protocol.ErrorContentOnlyUnavailable,
		provider.ProtocolIncompatible:   protocol.ErrorProviderProtocolFailure,
	}
	for state, expectedCode := range readinessCodes {
		readiness := provider.Readiness{State: state, Provider: provider.NamePi, Model: "model"}
		mapped, unavailable := MapReadiness(readiness)
		require.True(t, unavailable)
		require.Equal(t, expectedCode, mapped.Code())
	}
	mapped, unavailable := MapReadiness(provider.Readiness{State: provider.Ready, Provider: provider.NamePi, Model: "model"})
	require.False(t, unavailable)
	require.False(t, mapped.Valid())
	mapped, unavailable = MapReadiness(provider.Readiness{State: provider.Ready, Provider: provider.Name("other"), Model: "model"})
	require.True(t, unavailable)
	require.Equal(t, protocol.ErrorProviderProtocolFailure, mapped.Code())
	require.Equal(t, protocol.ErrorProviderProtocolFailure, MapProviderError(provider.ProviderError{}).Code())

	cause := errors.New("/private/path native secret")
	mapped = MapError(cause)
	require.Equal(t, protocol.ErrorProviderProtocolFailure, mapped.Code())
	require.NotContains(t, mapped.Error(), cause.Error())

	var browserCoded interface {
		BrowserErrorCode() protocol.BrowserErrorCode
	} = mapped
	require.Equal(t, protocol.ErrorProviderProtocolFailure, browserCoded.BrowserErrorCode())
}

func TestConversionsRequireCanonicalAuthorizedOriginAndCopyContextBytes(t *testing.T) {
	resource := testResource(testID('A'))
	page := testContext(resource)
	identity := ConnectIdentity{Origin: "https://example.com", Provider: protocol.ProviderPi, Resource: resource, ContextDigest: page.Digest}
	state, err := ConnectIdentityToState(identity, "https://example.com")
	require.NoError(t, err)
	require.Equal(t, "https://example.com", state.Origin)
	require.NoError(t, state.Validate())

	_, err = ConnectIdentityToState(identity, "https://EXAMPLE.com")
	require.Error(t, err)
	_, err = ConnectIdentityToState(ConnectIdentity{Origin: "https://EXAMPLE.com", Provider: protocol.ProviderPi, Resource: resource, ContextDigest: page.Digest}, "https://example.com")
	require.Error(t, err)

	loopbackIdentity := identity
	loopbackIdentity.Origin = "http://127.0.0.1:8080"
	loopbackState, err := ConnectIdentityToState(loopbackIdentity, loopbackIdentity.Origin)
	require.NoError(t, err)
	require.Equal(t, loopbackIdentity.Origin, loopbackState.Origin)
	require.NoError(t, loopbackState.Validate())
	loopbackPage := page
	loopbackPage.URL = "http://127.0.0.1:8080/board/readme"
	_, err = PageContextToProvider(loopbackPage, loopbackIdentity, loopbackIdentity.Origin)
	require.NoError(t, err)
	loopbackPage.URL = "http://localhost:8080/board/readme"
	_, err = PageContextToProvider(loopbackPage, loopbackIdentity, loopbackIdentity.Origin)
	require.Error(t, err)
	defaultPortIdentity := identity
	defaultPortIdentity.Origin = "http://127.0.0.1"
	for _, pageURL := range []string{"http://127.0.0.1:80/board/readme", "http://127.0.0.1:080/board/readme"} {
		loopbackPage.URL = pageURL
		_, err = PageContextToProvider(loopbackPage, defaultPortIdentity, defaultPortIdentity.Origin)
		require.Error(t, err)
	}

	converted, err := PageContextToProvider(page, identity, identity.Origin)
	require.NoError(t, err)
	require.Equal(t, page.Digest, converted.Digest)
	require.Equal(t, page.Resource.CreatedAt, converted.Resource.CreatedAt)
	require.Equal(t, page.Resource.UpdatedAt, converted.Resource.UpdatedAt)
	require.Equal(t, *page.Resource.ExpiresAt, *converted.Resource.ExpiresAt)
	back, err := PageContextFromProvider(converted, identity, identity.Origin)
	require.NoError(t, err)
	require.Equal(t, page, back)
	converted.Source[0] = 'X'
	converted.CreatorContext[0] = 'X'
	require.Equal(t, byte('#'), page.Source[0])
	require.Equal(t, byte('c'), page.CreatorContext[0])

	_, err = PageContextToProvider(page, identity, "https://other.example.com")
	require.Error(t, err)
	mismatch := page
	mismatch.Resource.UpdatedAt = mismatch.Resource.UpdatedAt.Add(time.Second)
	_, err = PageContextToProvider(mismatch, identity, identity.Origin)
	require.Error(t, err)
	mismatch = page
	mismatch.Digest = agent.CalculateContextDigest([]byte(mismatch.Source+"changed"), []byte(mismatch.CreatorContext))
	mismatch.Source += "changed"
	_, err = PageContextToProvider(mismatch, identity, identity.Origin)
	require.Error(t, err)
	page.URL = "http://example.com/board/readme"
	_, err = PageContextToProvider(page, identity, identity.Origin)
	require.Error(t, err)
	page.URL = "https://other.example.com/board/readme"
	_, err = PageContextToProvider(page, identity, identity.Origin)
	require.Error(t, err)
}

func TestHTMLConversionsPreserveKindSourceAndStateIdentity(t *testing.T) {
	resource := testResource(testID('H'))
	resource.Kind = protocol.ResourceHTML
	page := testContext(resource)
	page.Source = "<!doctype html><title>Exact</title>\x00"
	page.Digest, _ = agent.CalculateContextDigestForKind(agent.ResourceKindHTML, []byte(page.Source), []byte(page.CreatorContext))
	identity := ConnectIdentity{Origin: "https://example.com", Provider: protocol.ProviderCodex, Resource: resource, ContextDigest: page.Digest}

	stateIdentity, err := ConnectIdentityToState(identity, identity.Origin)
	require.NoError(t, err)
	require.Equal(t, statepkg.ResourceHTML, stateIdentity.Kind)

	converted, err := PageContextToProvider(page, identity, identity.Origin)
	require.NoError(t, err)
	require.Equal(t, provider.ResourceHTML, converted.Resource.Kind)
	require.Equal(t, []byte(page.Source), converted.Source)
	back, err := PageContextFromProvider(converted, identity, identity.Origin)
	require.NoError(t, err)
	require.Equal(t, page, back)
}

func TestConnectIdentityPreservesEachProvider(t *testing.T) {
	resource := testResource(testID('Z'))
	page := testContext(resource)
	for _, test := range []struct {
		wire   protocol.ProviderName
		domain provider.Name
	}{
		{protocol.ProviderPi, provider.NamePi},
		{protocol.ProviderCodex, provider.NameCodex},
		{protocol.ProviderCursor, provider.NameCursor},
	} {
		identity := ConnectIdentity{Origin: "https://example.com", Provider: test.wire, Resource: resource, ContextDigest: page.Digest}
		state, err := ConnectIdentityToState(identity, identity.Origin)
		require.NoError(t, err)
		require.Equal(t, test.domain, state.Provider)
	}
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
	first := protocol.Event{APIVersion: protocol.APIVersion, EventID: sequenceID(1), ConversationID: conversation, Type: protocol.EventQueue, Timestamp: now, Payload: protocol.QueuePayload{Items: []protocol.QueueItem{{TurnID: testID('F'), MessageID: testID('G'), Content: protocol.TextContent("✓"), Settings: nil}}}}
	encoded, err := protocol.EncodeEvent(first)
	require.NoError(t, err)
	require.NoError(t, log.Append(first))
	require.Equal(t, len(encoded), log.Bytes())

	targeted := first
	targeted.EventID = sequenceID(2)
	require.NoError(t, log.AppendForClient(clientA, targeted))
	broadcast := first
	broadcast.EventID = sequenceID(3)
	require.NoError(t, log.Append(broadcast))

	events, err := log.Replay(clientB, first.EventID)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, broadcast.EventID, events[0].EventID)
	_, err = log.Replay(clientB, targeted.EventID)
	require.ErrorIs(t, err, ErrReplayCursorMissing)
	events, err = log.Replay(clientA, first.EventID)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.NotEqual(t, first.EventID, events[0].EventID) // cursor is strictly later

	// A large escaped payload proves the actual encoded wire size is used.
	for index := 4; index < 1600; index++ {
		event := first
		event.EventID = sequenceID(uint64(index))
		event.Type = protocol.EventActivity
		event.Payload = protocol.ActivityPayload{Kind: protocol.ActivityStatus, Summary: strings.Repeat("✓\\\"", 1200)}
		if err := log.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	require.LessOrEqual(t, log.Len(), MaxReplayEvents)
	require.LessOrEqual(t, log.Bytes(), MaxReplayBytes)
	_, err = log.Replay(clientA, first.EventID)
	require.ErrorIs(t, err, ErrReplayCursorEvicted)
}

func TestReplayLogReturnsStablePayloadCopies(t *testing.T) {
	log := NewReplayLog()
	active := &protocol.ActiveWork{WorkID: testID('D'), Kind: protocol.ActiveWorkTurn, State: protocol.ActiveWorkRunning}
	skillsUnavailable := protocol.SkillsUnavailable
	event := protocol.Event{APIVersion: protocol.APIVersion, EventID: sequenceID(100), ConversationID: testID('A'), Type: protocol.EventSnapshot, Timestamp: testTime(), Payload: protocol.SnapshotPayload{Lifecycle: protocol.LifecycleResponding, Queue: []protocol.QueueItem{{TurnID: testID('B'), MessageID: testID('C'), Content: protocol.TextContent("original"), Settings: nil}}, ContextState: protocol.ContextPending, ActiveWork: active, SettingsState: nil, EffectiveSettings: nil, Catalog: []protocol.CatalogModel{}, SkillsState: &skillsUnavailable, Skills: []protocol.SkillDescriptor{}, BusyPolicy: protocol.BusyTurnQueue, ComposerAdmission: protocol.ComposerQueue}}
	require.NoError(t, log.Append(event))
	payload := event.Payload.(protocol.SnapshotPayload)
	payload.Queue[0].Content.Parts[0].Text = "changed"
	payload.ActiveWork.WorkID = testID('E')

	lifecycleWork := &protocol.ActiveWork{WorkID: testID('H'), Kind: protocol.ActiveWorkTurn, State: protocol.ActiveWorkRunning}
	lifecycle := protocol.Event{APIVersion: protocol.APIVersion, EventID: sequenceID(101), ConversationID: testID('A'), Type: protocol.EventLifecycle, Timestamp: testTime(), Payload: protocol.LifecyclePayload{State: protocol.LifecycleResponding, ActiveWork: lifecycleWork}}
	require.NoError(t, log.Append(lifecycle))
	lifecycle.Payload.(protocol.LifecyclePayload).ActiveWork.WorkID = testID('I')

	got, err := log.Replay(testID('G'), "")
	require.NoError(t, err)
	stored := got[0].Payload.(protocol.SnapshotPayload)
	require.Equal(t, "original", stored.Queue[0].Content.Parts[0].Text)
	require.Equal(t, testID('D'), stored.ActiveWork.WorkID)
	require.Equal(t, testID('H'), got[1].Payload.(protocol.LifecyclePayload).ActiveWork.WorkID)
	stored.ActiveWork.WorkID = testID('F')
	got[1].Payload.(protocol.LifecyclePayload).ActiveWork.WorkID = testID('J')
	again, err := log.Replay(testID('G'), "")
	require.NoError(t, err)
	require.Equal(t, testID('D'), again[0].Payload.(protocol.SnapshotPayload).ActiveWork.WorkID)
	require.Equal(t, testID('H'), again[1].Payload.(protocol.LifecyclePayload).ActiveWork.WorkID)
	require.NotContains(t, string(mustEncodeEvent(t, got[0])), "creator_context")
}

func TestReplayDeepClonesSkillLimitsForValueAndPointerPayloads(t *testing.T) {
	log := NewReplayLog()
	limit := 2
	ready := protocol.SkillsReady
	valueSnapshot := protocol.SnapshotPayload{Lifecycle: protocol.LifecycleReady, Queue: []protocol.QueueItem{}, ContextState: protocol.ContextPending, SupportsArchiveDelete: true, Catalog: []protocol.CatalogModel{}, SkillsState: &ready, Skills: []protocol.SkillDescriptor{}, MaxSelectedSkills: &limit, BusyPolicy: protocol.BusyTurnQueue, ComposerAdmission: protocol.ComposerSubmit}
	pointerCatalog := &protocol.SkillCatalogPayload{State: protocol.SkillsReady, Skills: []protocol.SkillDescriptor{}, MaxSelectedSkills: &limit}
	events := []protocol.Event{
		{APIVersion: protocol.APIVersion, EventID: sequenceID(180), ConversationID: testID('A'), Type: protocol.EventSnapshot, Timestamp: testTime(), Payload: valueSnapshot},
		{APIVersion: protocol.APIVersion, EventID: sequenceID(181), ConversationID: testID('A'), Type: protocol.EventSkillCatalog, Timestamp: testTime(), Payload: pointerCatalog},
	}
	for _, event := range events {
		require.NoError(t, log.Append(event))
	}
	limit = 9
	first, err := log.Replay(testID('G'), "")
	require.NoError(t, err)
	require.Equal(t, 2, *first[0].Payload.(protocol.SnapshotPayload).MaxSelectedSkills)
	require.True(t, first[0].Payload.(protocol.SnapshotPayload).SupportsArchiveDelete)
	require.Equal(t, 2, *first[1].Payload.(*protocol.SkillCatalogPayload).MaxSelectedSkills)
	*first[0].Payload.(protocol.SnapshotPayload).MaxSelectedSkills = 7
	*first[1].Payload.(*protocol.SkillCatalogPayload).MaxSelectedSkills = 8
	second, err := log.Replay(testID('G'), "")
	require.NoError(t, err)
	require.Equal(t, 2, *second[0].Payload.(protocol.SnapshotPayload).MaxSelectedSkills)
	require.Equal(t, 2, *second[1].Payload.(*protocol.SkillCatalogPayload).MaxSelectedSkills)
}

func TestReplayClonesPreserveRequiredEmptySlices(t *testing.T) {
	log := NewReplayLog()
	conversation := testID('K')
	client := testID('L')
	skillsUnavailable := protocol.SkillsUnavailable
	payloads := []protocol.EventPayload{
		protocol.SnapshotPayload{Lifecycle: protocol.LifecycleReady, Queue: []protocol.QueueItem{}, ContextState: protocol.ContextPending, SettingsState: nil, EffectiveSettings: nil, Catalog: []protocol.CatalogModel{}, SkillsState: &skillsUnavailable, Skills: []protocol.SkillDescriptor{}, BusyPolicy: protocol.BusyTurnQueue, ComposerAdmission: protocol.ComposerSubmit},
		protocol.QueuePayload{Items: []protocol.QueueItem{}},
		protocol.TimelinePayload{CommandID: testID('M'), Items: []protocol.TimelineItem{}, NextCursor: nil},
		protocol.HistoryPayload{CommandID: testID('N'), Items: []protocol.ArchiveItem{}, NextCursor: nil},
	}
	for index, payload := range payloads {
		event := protocol.Event{APIVersion: protocol.APIVersion, EventID: sequenceID(uint64(index + 200)), ConversationID: conversation, Type: payload.EventType(), Timestamp: testTime(), Payload: payload}
		require.NoError(t, log.Append(event))
	}
	replayed, err := log.Replay(client, "")
	require.NoError(t, err)
	require.Len(t, replayed, len(payloads))
	for _, event := range replayed {
		_, err := protocol.EncodeEvent(event)
		require.NoError(t, err)
	}
}

func TestReplayEvictionClassificationTracksRecentVisibilityWithBoundedMemory(t *testing.T) {
	log := NewReplayLog()
	clientA := testID('A')
	clientB := testID('B')
	conversation := testID('C')
	recentTargetedIndex := MaxReplayEvents * 2
	for index := 1; index <= MaxReplayEvents*3; index++ {
		event := protocol.Event{APIVersion: protocol.APIVersion, EventID: sequenceID(uint64(index)), ConversationID: conversation, Type: protocol.EventActivity, Timestamp: testTime(), Payload: protocol.ActivityPayload{Kind: protocol.ActivityStatus, Summary: strings.Repeat("x", 5000)}}
		target := ""
		if index == recentTargetedIndex {
			target = clientA
		}
		if target == "" {
			require.NoError(t, log.Append(event))
		} else {
			require.NoError(t, log.AppendForClient(target, event))
		}
	}
	require.Equal(t, MaxReplayEvents, log.evictedLen())
	recentEvicted := sequenceID(uint64(recentTargetedIndex))
	_, err := log.Replay(clientB, recentEvicted)
	require.ErrorIs(t, err, ErrReplayCursorMissing)
	_, err = log.Replay(clientA, recentEvicted)
	require.ErrorIs(t, err, ErrReplayCursorEvicted)
}

func TestQueueFIFOBoundsDuplicatesEditRemoveAndContextErasure(t *testing.T) {
	queue := NewQueue()
	resource := provider.Resource{Kind: provider.ResourceMarkdown, ID: testID('H'), CreatedAt: testTime(), UpdatedAt: testTime().Add(time.Minute)}
	context := testProviderContext(resource)
	turnA := QueuedTurn{TurnID: testID('I'), MessageID: testID('J'), Content: provider.TextMessage("first"), Context: &context}
	turnB := QueuedTurn{TurnID: testID('K'), MessageID: testID('L'), Content: provider.TextMessage("second")}
	require.NoError(t, queue.Enqueue(turnA))
	require.NoError(t, queue.Enqueue(turnB))
	require.Equal(t, len("first")+len("second"), queue.Bytes())
	require.Equal(t, []protocol.QueueItem{{TurnID: turnA.TurnID, MessageID: turnA.MessageID, Content: protocol.TextContent("first")}, {TurnID: turnB.TurnID, MessageID: turnB.MessageID, Content: protocol.TextContent("second")}}, queue.Items())
	require.ErrorIs(t, queue.Enqueue(QueuedTurn{TurnID: turnA.TurnID, MessageID: testID('M'), Content: provider.TextMessage("duplicate")}), ErrQueueDuplicateID)
	require.ErrorIs(t, queue.Enqueue(QueuedTurn{TurnID: testID('N'), MessageID: turnB.MessageID, Content: provider.TextMessage("duplicate")}), ErrQueueDuplicateID)
	secondContext := testProviderContext(resource)
	require.ErrorIs(t, queue.Enqueue(QueuedTurn{TurnID: testID('M'), MessageID: testID('N'), Content: provider.TextMessage("second context"), Context: &secondContext}), ErrQueueContextConflict)
	require.NoError(t, queue.Edit(turnA.MessageID, provider.TextMessage("edited")))
	require.Equal(t, "edited", queue.Items()[0].Content.Parts[0].Text)
	require.ErrorIs(t, queue.Edit(testID('O'), provider.TextMessage("missing")), ErrQueueItemNotFound)

	owned := queue.items[0].context
	require.NotNil(t, owned)
	require.NotEmpty(t, owned.Source)
	require.NoError(t, queue.Remove(turnA.MessageID))
	require.Nil(t, owned.Source)
	require.Nil(t, owned.CreatorContext)
	dequeued, ok := queue.Dequeue()
	require.True(t, ok)
	require.Equal(t, turnB.TurnID, dequeued.TurnID)
	require.True(t, queue.Empty())

	require.NoError(t, queue.Enqueue(QueuedTurn{TurnID: testID('M'), MessageID: testID('N'), Content: provider.TextMessage("second context"), Context: &secondContext}))
	secondOwned := queue.items[0].context
	queue.Clear()
	require.True(t, queue.Empty())
	require.Nil(t, secondOwned.Source)
	require.Nil(t, secondOwned.CreatorContext)
}

func TestQueueBoundsAreByteAndItemExact(t *testing.T) {
	queue := NewQueue()
	for index := 0; index < MaxQueueItems; index++ {
		require.NoError(t, queue.Enqueue(QueuedTurn{TurnID: sequenceID(uint64(index + 1)), MessageID: sequenceID(uint64(index + 1000)), Content: provider.TextMessage("x")}))
	}
	require.ErrorIs(t, queue.Enqueue(QueuedTurn{TurnID: sequenceID(10000), MessageID: sequenceID(10001), Content: provider.TextMessage("x")}), ErrQueueFull)

	queue = NewQueue()
	message := strings.Repeat("界", 49152/len("界"))
	require.NoError(t, queue.Enqueue(QueuedTurn{TurnID: testID('P'), MessageID: testID('Q'), Content: provider.TextMessage(message)}))
	require.NoError(t, queue.Enqueue(QueuedTurn{TurnID: testID('R'), MessageID: testID('S'), Content: provider.TextMessage(message)}))
	require.ErrorIs(t, queue.Enqueue(QueuedTurn{TurnID: testID('W'), MessageID: testID('X'), Content: provider.TextMessage("x")}), ErrQueueFull)
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

func TestEventFactoryRejectsTypedNilDependenciesAndPayload(t *testing.T) {
	var ids *testIDGenerator
	_, err := NewEventFactory(testID('T'), ids, testClock{now: testTime()})
	require.ErrorIs(t, err, ErrNilEventIDGenerator)
	var clock *nilClock
	_, err = NewEventFactory(testID('T'), &testIDGenerator{}, clock)
	require.ErrorIs(t, err, ErrNilEventClock)

	factory, err := NewEventFactory(testID('T'), &testIDGenerator{ids: []string{testID('U')}}, testClock{now: testTime()})
	require.NoError(t, err)
	var payload *protocol.CompletionPayload
	_, err = factory.New(payload)
	require.Error(t, err)
}

type nilClock struct{}

func (*nilClock) Now() time.Time { return time.Time{} }

func TestEventFactoryValidatesAndConvertsEveryProviderEvent(t *testing.T) {
	now := testTime()
	providerEvents := []provider.Event{
		provider.NewUserMessageEvent(testID('A'), testID('B'), provider.TextMessage("question"), now),
		provider.NewAssistantDeltaEvent(testID('A'), testID('C'), "part"),
		provider.NewAssistantMessageEvent(testID('A'), testID('C'), "answer", now),
		provider.NewActivityEvent(testID('A'), provider.ActivityStatus, "working"),
		provider.NewBlockedEvent(testID('A'), provider.BlockedTool),
		provider.NewCompletionEvent(testID('A')),
		provider.NewInterruptionEvent(testID('A'), provider.InterruptionRequested),
		provider.NewTerminalFailureEvent(testID('A'), provider.NewProviderError(provider.ErrorProtocolFailure)),
	}
	expected := []protocol.EventType{
		protocol.EventUserMessage,
		protocol.EventAssistantDelta,
		protocol.EventAssistantMessage,
		protocol.EventActivity,
		protocol.EventBlocked,
		protocol.EventCompletion,
		protocol.EventInterruption,
		protocol.EventError,
	}
	ids := make([]string, len(providerEvents))
	for index := range ids {
		ids[index] = sequenceID(uint64(index + 1))
	}
	factory, err := NewEventFactory(testID('T'), &testIDGenerator{ids: ids}, testClock{now: now})
	require.NoError(t, err)
	for index, providerEvent := range providerEvents {
		event, err := factory.FromProvider(providerEvent)
		require.NoError(t, err)
		require.Equal(t, expected[index], event.Type)
	}

	malformed := provider.NewCompletionEvent(testID('V'))
	malformed.Text = "forbidden provider field"
	_, err = factory.FromProvider(malformed)
	var brokerFailure BrokerError
	require.ErrorAs(t, err, &brokerFailure)
	require.Equal(t, protocol.ErrorProviderMalformedStream, brokerFailure.Code())
}

func TestEventFactoryUsesInjectedIDAndClockAndValidates(t *testing.T) {
	conversation := testID('T')
	ids := &testIDGenerator{ids: []string{testID('U')}}
	clock := testClock{now: testTime()}
	factory, err := NewEventFactory(conversation, ids, clock)
	require.NoError(t, err)
	event, err := factory.New(protocol.CompletionPayload{TurnID: testID('V')})
	require.NoError(t, err)
	require.Equal(t, testID('U'), event.EventID)
	require.Equal(t, conversation, event.ConversationID)
	require.Equal(t, testTime(), event.Timestamp)
	require.Equal(t, protocol.EventCompletion, event.Type)
}

func mustEncodeEvent(t *testing.T, event protocol.Event) []byte {
	encoded, err := protocol.EncodeEvent(event)
	require.NoError(t, err)
	return encoded
}

// Keep the interface assertions close to the deterministic helpers used by
// the factory tests.
var _ common.IDGenerator = (*testIDGenerator)(nil)
var _ common.Clock = testClock{}
