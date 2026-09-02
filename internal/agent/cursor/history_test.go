package cursor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
)

const turnID = "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4"
const messageID = "eHl6eHl6eHl6eHl6eHl6eHl6eHl6eHl6"

func TestLoadReplayExactEnvelopeAndReconciliation(t *testing.T) {
	d, _, root := testDriver(t)
	s := newSession(d, &runtime{caps: capabilities{Image: true}}, root)
	s.beginReplay()
	request := provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("exact reader")}
	encoded, err := provider.Build(request, provider.PolicyConfigured)
	if err != nil {
		t.Fatal(err)
	}
	sendReplay(t, s, "user_message_chunk", string(encoded))
	sendReplay(t, s, "agent_message_chunk", "answer")
	if err = s.finishOpen(openResult{SessionID: "native", ConfigOptions: testOptions("model-a")}, true); err != nil {
		t.Fatal(err)
	}
	page, err := s.History(context.Background(), provider.HistoryRequest{})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("%v %+v", err, page)
	}
	if page.Items[1].Content.Parts[0].Text != "exact reader" || page.Items[0].Text != "answer" {
		t.Fatalf("%+v", page.Items)
	}
	state, err := s.Reconcile(context.Background(), provider.TurnReference{TurnID: turnID})
	if err != nil || state != provider.TurnAccepted {
		t.Fatalf("%v %s", err, state)
	}
	absent := "QUJDREVGR0hJSktMTU5PUFFSU1RVVldY"
	state, err = s.Reconcile(context.Background(), provider.TurnReference{TurnID: absent})
	if err != nil || state != provider.TurnNotAccepted {
		t.Fatalf("%v %s", err, state)
	}
}
func TestLoadReplaySuppressesCursorClosedStreamArtifact(t *testing.T) {
	d, _, root := testDriver(t)
	s := newSession(d, &runtime{caps: capabilities{}}, root)
	s.beginReplay()
	request := provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("reader")}
	encoded, err := provider.Build(request, provider.PolicyConfigured)
	if err != nil {
		t.Fatal(err)
	}
	sendReplay(t, s, "user_message_chunk", string(encoded))
	sendReplay(t, s, "agent_message_chunk", "answer")
	sendReplay(t, s, "agent_message_chunk", "\n"+cursorClosedStreamArtifact+"\n")
	if err = s.finishOpen(openResult{SessionID: "native", ConfigOptions: testOptions("model-a")}, true); err != nil {
		t.Fatal(err)
	}
	page, err := s.History(context.Background(), provider.HistoryRequest{})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("history = %+v, %v", page, err)
	}
	for _, item := range page.Items {
		if strings.Contains(item.Text, "RetriableError") {
			t.Fatalf("runtime artifact escaped through replay: %+v", page.Items)
		}
	}
}

func TestHistoryReturnsDeeplyIsolatedMessageContent(t *testing.T) {
	d, _, root := testDriver(t)
	s := newSession(d, &runtime{caps: capabilities{}}, root)
	s.beginReplay()
	content := provider.MessageContent{Parts: []provider.MessagePart{
		{Kind: provider.MessagePartText, Text: "reader"},
		{Kind: provider.MessagePartSkill, Skill: &provider.SkillInvocation{ID: turnID, Name: "review"}},
	}}
	encoded, err := provider.Build(provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: content}, provider.PolicyConfigured)
	if err != nil {
		t.Fatal(err)
	}
	sendReplay(t, s, "user_message_chunk", string(encoded))
	if err = s.finishOpen(openResult{SessionID: "native", ConfigOptions: testOptions("model-a")}, true); err != nil {
		t.Fatal(err)
	}
	first, err := s.History(context.Background(), provider.HistoryRequest{})
	if err != nil || len(first.Items) != 1 || len(first.Items[0].Content.Parts) != 2 {
		t.Fatalf("first history = %+v, %v", first, err)
	}
	first.Items[0].Content.Parts[0].Text = "mutated"
	first.Items[0].Content.Parts[1].Skill.Name = "mutated skill"
	first.Items[0].Content.Parts = append(first.Items[0].Content.Parts, provider.MessagePart{Kind: provider.MessagePartText, Text: "extra"})
	second, err := s.History(context.Background(), provider.HistoryRequest{})
	if err != nil || len(second.Items) != 1 || len(second.Items[0].Content.Parts) != 2 || second.Items[0].Content.Parts[0].Text != "reader" || second.Items[0].Content.Parts[1].Skill.Name != "review" {
		t.Fatalf("second history was mutated = %+v, %v", second, err)
	}
}

func TestLoadReplayRejectsTransformedEnvelope(t *testing.T) {
	d, _, root := testDriver(t)
	s := newSession(d, &runtime{caps: capabilities{}}, root)
	s.beginReplay()
	sendReplay(t, s, "user_message_chunk", "reader text transformed by native runtime")
	if err := s.finishOpen(openResult{SessionID: "native", ConfigOptions: testOptions("model-a")}, true); err == nil {
		t.Fatal("transformed replay accepted")
	}
}
func sendReplay(t *testing.T, s *Session, kind, text string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"sessionId": "native", "update": map[string]any{"sessionUpdate": kind, "content": map[string]any{"type": "text", "text": text}}})
	if err != nil {
		t.Fatal(err)
	}
	s.update(raw)
}

func TestMalformedAcceptedUpdatePoisonsAllLaterTerminalOutput(t *testing.T) {
	d, _, root := testDriver(t)
	s := newSession(d, &runtime{caps: capabilities{}}, root)
	if err := s.finishOpen(openResult{SessionID: "native", ConfigOptions: testOptions("model-a")}, false); err != nil {
		t.Fatal(err)
	}
	turn := &activePrompt{request: provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("reader")}, accepted: make(chan struct{}), done: make(chan error, 1), assistantID: safeID("assistant", turnID), permissionGateOpen: true, tools: make(map[string]cachedTool)}
	s.active = turn
	s.accept(turn)
	<-s.events
	s.update(json.RawMessage(`{"sessionId":"native","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":""}}}`))
	s.finishPrompt(turn, "end_turn")
	first := <-s.events
	if first.Kind != provider.EventTerminalFailure {
		t.Fatalf("first terminal = %s", first.Kind)
	}
	select {
	case event := <-s.events:
		t.Fatalf("poisoned turn emitted later event %s", event.Kind)
	default:
	}
}

func TestOversizedThoughtPoisonsInsteadOfBeingRetainedOrIgnored(t *testing.T) {
	d, _, root := testDriver(t)
	s := newSession(d, &runtime{caps: capabilities{}}, root)
	if err := s.finishOpen(openResult{SessionID: "native", ConfigOptions: testOptions("model-a")}, false); err != nil {
		t.Fatal(err)
	}
	turn := &activePrompt{request: provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("reader")}, accepted: make(chan struct{}), done: make(chan error, 1), assistantID: safeID("assistant", turnID), permissionGateOpen: true, tools: make(map[string]cachedTool)}
	s.active = turn
	raw, _ := json.Marshal(map[string]any{"sessionId": "native", "update": map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": strings.Repeat("x", provider.MaxDeltaBytes+1)}}})
	s.update(raw)
	s.mu.Lock()
	poisoned := s.active == nil && turn.poisoned
	s.mu.Unlock()
	if !poisoned {
		t.Fatal("oversized thought did not quarantine prompt")
	}
}
