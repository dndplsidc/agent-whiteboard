package cursor

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
)

func replaySession(t *testing.T) *Session {
	t.Helper()
	d, _, root := testDriver(t)
	s := newSession(d, &runtime{caps: capabilities{}}, root)
	s.beginReplay()
	return s
}

func replayEnvelope(t *testing.T, turn, message, text string) string {
	t.Helper()
	encoded, err := provider.Build(provider.TurnRequest{TurnID: turn, MessageID: message, Content: provider.TextMessage(text)}, provider.PolicyConfigured)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func finishReplay(t *testing.T, s *Session) error {
	t.Helper()
	return s.finishOpen(openResult{SessionID: "native", ConfigOptions: testOptions("model-a")}, true)
}

func TestReplayGateCanonicalityAndBoundary(t *testing.T) {
	t.Run("unavailable until successful load response", func(t *testing.T) {
		s := replaySession(t)
		sendReplay(t, s, "user_message_chunk", replayEnvelope(t, turnID, messageID, "reader"))
		state, err := s.Reconcile(context.Background(), provider.TurnReference{TurnID: turnID})
		if state != provider.TurnUnknown || err == nil {
			t.Fatalf("before boundary = %s, %v", state, err)
		}
		if err = finishReplay(t, s); err != nil {
			t.Fatal(err)
		}
		state, err = s.Reconcile(context.Background(), provider.TurnReference{TurnID: turnID})
		if state != provider.TurnAccepted || err != nil {
			t.Fatalf("after boundary = %s, %v", state, err)
		}
	})
	t.Run("noncanonical reader JSON", func(t *testing.T) {
		s := replaySession(t)
		envelope := replayEnvelope(t, turnID, messageID, "reader")
		canonical := `{"parts":[{"type":"text","text":"reader"}]}`
		transformed := `{ "parts":[{"type":"text","text":"reader"}]}`
		envelope = strings.Replace(envelope, "reader-content-untrusted "+stringInt(len(canonical))+"\n"+canonical, "reader-content-untrusted "+stringInt(len(transformed))+"\n"+transformed, 1)
		sendReplay(t, s, "user_message_chunk", envelope)
		if err := finishReplay(t, s); err == nil {
			t.Fatal("noncanonical JSON accepted")
		}
	})
	t.Run("truncated and historical envelopes", func(t *testing.T) {
		for _, mutate := range []func(string) string{
			func(v string) string { return v[:len(v)-1] },
			func(v string) string { return strings.Replace(v, provider.Header, "agent-whiteboard-turn-v3\n", 1) },
		} {
			s := replaySession(t)
			sendReplay(t, s, "user_message_chunk", mutate(replayEnvelope(t, turnID, messageID, "reader")))
			if err := finishReplay(t, s); err == nil {
				t.Fatal("invalid envelope accepted")
			}
		}
	})
}

func stringInt(value int) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestReplayRejectsIdentityOrderingAndSessionContradictions(t *testing.T) {
	secondTurn := "QUJDREVGR0hJSktMTU5PUFFSU1RVVldY"
	secondMessage := "MTIzNDU2Nzg5MGFiY2RlZjEyMzQ1Njc4"
	tests := map[string]func(*testing.T, *Session){
		"duplicate turn": func(t *testing.T, s *Session) {
			sendReplay(t, s, "user_message_chunk", replayEnvelope(t, turnID, messageID, "one"))
			sendReplay(t, s, "user_message_chunk", replayEnvelope(t, turnID, secondMessage, "two"))
		},
		"colliding message": func(t *testing.T, s *Session) {
			sendReplay(t, s, "user_message_chunk", replayEnvelope(t, turnID, messageID, "one"))
			sendReplay(t, s, "user_message_chunk", replayEnvelope(t, secondTurn, messageID, "two"))
		},
		"assistant before user": func(t *testing.T, s *Session) { sendReplay(t, s, "agent_message_chunk", "answer") },
		"thought before user":   func(t *testing.T, s *Session) { sendReplay(t, s, "agent_thought_chunk", "secret") },
		"tool before user": func(t *testing.T, s *Session) {
			sendReplayUpdate(t, s, "native", map[string]any{"sessionUpdate": "tool_call", "toolCallId": "native-secret", "title": "tool", "kind": "read"})
		},
		"tool update without creation": func(t *testing.T, s *Session) {
			sendReplay(t, s, "user_message_chunk", replayEnvelope(t, turnID, messageID, "one"))
			sendReplayUpdate(t, s, "native", map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "missing", "status": "completed"})
		},
		"tool bounds": func(t *testing.T, s *Session) {
			sendReplay(t, s, "user_message_chunk", replayEnvelope(t, turnID, messageID, "one"))
			sendReplayUpdate(t, s, "native", map[string]any{"sessionUpdate": "tool_call", "toolCallId": "tool", "title": strings.Repeat("x", provider.MaxTitleBytes+1), "kind": "read"})
		},
		"terminal tool regression": func(t *testing.T, s *Session) {
			sendReplay(t, s, "user_message_chunk", replayEnvelope(t, turnID, messageID, "one"))
			sendReplayUpdate(t, s, "native", map[string]any{"sessionUpdate": "tool_call", "toolCallId": "tool", "title": "tool", "kind": "read", "status": "completed"})
			sendReplayUpdate(t, s, "native", map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "tool", "status": "running"})
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			s := replaySession(t)
			setup(t, s)
			if err := finishReplay(t, s); err == nil {
				t.Fatal("contradictory replay accepted")
			}
		})
	}
	t.Run("session mismatch", func(t *testing.T) {
		s := replaySession(t)
		sendReplay(t, s, "user_message_chunk", replayEnvelope(t, turnID, messageID, "one"))
		sendReplayUpdate(t, s, "other", map[string]any{"sessionUpdate": "future_standard_update"})
		if err := finishReplay(t, s); err == nil {
			t.Fatal("session change accepted")
		}
	})
	t.Run("update overflow", func(t *testing.T) {
		s := replaySession(t)
		for i := 0; i <= 4096; i++ {
			sendReplayUpdate(t, s, "native", map[string]any{"sessionUpdate": "future_standard_update"})
		}
		if err := finishReplay(t, s); err == nil {
			t.Fatal("update overflow accepted")
		}
	})
}

func sendReplayUpdate(t *testing.T, s *Session, session string, update any) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"sessionId": session, "update": update})
	if err != nil {
		t.Fatal(err)
	}
	s.update(raw)
}

func TestReplayHistorySafeProjectionStableIDsAndPagination(t *testing.T) {
	s := replaySession(t)
	sendReplay(t, s, "user_message_chunk", replayEnvelope(t, turnID, messageID, "one"))
	sendReplay(t, s, "agent_thought_chunk", "private thought")
	sendReplayUpdate(t, s, "native", map[string]any{"sessionUpdate": "tool_call", "toolCallId": "native-tool-id", "title": "read", "kind": "read"})
	sendReplay(t, s, "agent_message_chunk", "answer ")
	sendReplay(t, s, "agent_message_chunk", "continued")
	secondTurn := "QUJDREVGR0hJSktMTU5PUFFSU1RVVldY"
	secondMessage := "MTIzNDU2Nzg5MGFiY2RlZjEyMzQ1Njc4"
	sendReplay(t, s, "user_message_chunk", replayEnvelope(t, secondTurn, secondMessage, "two"))
	if err := finishReplay(t, s); err != nil {
		t.Fatal(err)
	}
	page, err := s.History(context.Background(), provider.HistoryRequest{Limit: 2})
	if err != nil || len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatalf("page = %+v, %v", page, err)
	}
	if page.Items[1].Text != "answer continued" || page.Items[1].MessageID != safeID("history-assistant", turnID) {
		t.Fatalf("assistant = %+v", page.Items[1])
	}
	if !page.Items[0].CreatedAt.After(page.Items[1].CreatedAt) || page.Items[0].CreatedAt.Location() != time.UTC {
		t.Fatal("timestamps are not one-anchor monotonic UTC")
	}
	all, _ := json.Marshal(page)
	if bytes.Contains(all, []byte("private thought")) || bytes.Contains(all, []byte("native-tool-id")) {
		t.Fatalf("private replay data leaked: %s", all)
	}
	next, err := s.History(context.Background(), provider.HistoryRequest{Limit: 2, BeforeMessageID: page.NextCursor})
	if err != nil || len(next.Items) != 1 || next.Items[0].MessageID != messageID {
		t.Fatalf("next = %+v, %v", next, err)
	}
	if _, err = s.History(context.Background(), provider.HistoryRequest{BeforeMessageID: "bm90LXByZXNlbnQtaW4taGlzdG9yeQ"}); err == nil {
		t.Fatal("invalid cursor accepted")
	}
	state, err := s.Reconcile(context.Background(), provider.TurnReference{TurnID: secondTurn})
	if state != provider.TurnAccepted || err != nil {
		t.Fatalf("accepted without assistant = %s, %v", state, err)
	}
	state, err = s.Reconcile(context.Background(), provider.TurnReference{TurnID: "RkVEREJBOTg3NjU0MzIxMEFCQ0RFRkdI"})
	if state != provider.TurnNotAccepted || err != nil {
		t.Fatalf("absent = %s, %v", state, err)
	}
}

func TestReplayAcceptsExactReplacementEnvelope(t *testing.T) {
	s := replaySession(t)
	source, creator := []byte("replacement source"), []byte("replacement creator")
	at := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
	request := provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("replacement"), Context: &provider.PageContext{
		Revision: provider.ContextReplacement, Source: source, CreatorContext: creator, Title: "Board", URL: "https://example.test/board",
		Resource: provider.Resource{Kind: provider.ResourceMarkdown, ID: "QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFB", CreatedAt: at, UpdatedAt: at}, Digest: agent.CalculateContextDigest(source, creator),
	}}
	encoded, err := provider.Build(request, provider.PolicyConfigured)
	if err != nil {
		t.Fatal(err)
	}
	sendReplay(t, s, "user_message_chunk", string(encoded))
	if err = finishReplay(t, s); err != nil {
		t.Fatal(err)
	}
}

func TestReplayAcceptsMaximumConfiguredEnvelope(t *testing.T) {
	s := replaySession(t)
	source := bytes.Repeat([]byte{'s'}, provider.MaxSourceBytes)
	creator := bytes.Repeat([]byte{'c'}, provider.MaxCreatorContextBytes)
	at := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
	request := provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("reader"), Context: &provider.PageContext{
		Revision: provider.ContextInitial, Source: source, CreatorContext: creator, Title: "Board", URL: "https://example.test/board",
		Resource: provider.Resource{Kind: provider.ResourceMarkdown, ID: "QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFB", CreatedAt: at, UpdatedAt: at},
		Digest:   agent.CalculateContextDigest(source, creator),
	}}
	encoded, err := provider.Build(request, provider.PolicyConfigured)
	if err != nil {
		t.Fatal(err)
	}
	sendReplay(t, s, "user_message_chunk", string(encoded))
	if err = finishReplay(t, s); err != nil {
		t.Fatal(err)
	}
	page, err := s.History(context.Background(), provider.HistoryRequest{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Content.PlainText() != "reader" {
		t.Fatalf("history = %+v, %v", page, err)
	}
}

func TestCreateDoesNotPromotePreCreateReplay(t *testing.T) {
	d, _, root := testDriver(t)
	s := newSession(d, &runtime{caps: capabilities{}}, root)
	sendReplay(t, s, "user_message_chunk", replayEnvelope(t, turnID, messageID, "must disappear"))
	if err := s.finishOpen(openResult{SessionID: "native", ConfigOptions: testOptions("model-a")}, false); err != nil {
		t.Fatal(err)
	}
	page, err := s.History(context.Background(), provider.HistoryRequest{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("pre-create replay promoted: %+v, %v", page, err)
	}
}
