package cursor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
)

func newLiveToolSession(t *testing.T) (*Session, *activePrompt) {
	t.Helper()
	d, _, root := testDriver(t)
	s := newSession(d, &runtime{caps: capabilities{}}, root)
	if err := s.finishOpen(openResult{SessionID: "native", ConfigOptions: testOptions("model-a")}, false); err != nil {
		t.Fatal(err)
	}
	turn := &activePrompt{request: provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("reader")}, accepted: make(chan struct{}), rejected: make(chan struct{}), done: make(chan error, 1), result: make(chan promptResult, 1), assistantID: safeID("assistant", turnID), permissionGateOpen: true, phase: turnRunning, tools: map[string]cachedTool{}}
	s.mu.Lock()
	s.active = turn
	s.mu.Unlock()
	return s, turn
}

func TestToolCallUpdateMergesCachedStandardFieldsAndDropsNativeSummary(t *testing.T) {
	s, _ := newLiveToolSession(t)

	s.update([]byte(`{"sessionId":"native","update":{"sessionUpdate":"tool_call","toolCallId":"native-secret","title":"Read file","kind":"read","status":"pending","summary":"must not escape","detail":"must not escape"}}`))
	<-s.events // accepted user boundary
	first := <-s.events
	if first.Kind != provider.EventToolActivity || first.Tool == nil || first.Tool.Title != "Read file" || first.Tool.Summary != "" || first.Tool.Detail != "" {
		t.Fatalf("initial tool event = %#v", first)
	}

	s.update([]byte(`{"sessionId":"native","update":{"sessionUpdate":"tool_call_update","toolCallId":"native-secret","status":"completed","summary":"also ignored"}}`))
	second := <-s.events
	if second.Kind != provider.EventToolActivity || second.Tool == nil || second.Tool.Title != "Read file" || second.Tool.Kind != provider.ToolOther || second.Tool.Status != provider.ToolCompleted || second.Tool.Summary != "" || second.Tool.Detail != "" || strings.Contains(second.Tool.ID, "native") {
		t.Fatalf("merged tool event = %#v", second)
	}
}

func TestToolUpdateRejectsMissingIDUnknownEnumAndContradiction(t *testing.T) {
	cases := []struct{ name, initial, update string }{
		{"missing-id", ``, `{"sessionId":"native","update":{"sessionUpdate":"tool_call_update","status":"completed"}}`},
		{"unknown-kind", ``, `{"sessionId":"native","update":{"sessionUpdate":"tool_call","toolCallId":"x","title":"X","kind":"private"}}`},
		{"contradict-status", `{"sessionId":"native","update":{"sessionUpdate":"tool_call","toolCallId":"x","title":"X","kind":"read","status":"completed"}}`, `{"sessionId":"native","update":{"sessionUpdate":"tool_call_update","toolCallId":"x","status":"running"}}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			s, turn := newLiveToolSession(t)
			if test.initial != "" {
				s.update([]byte(test.initial))
				<-s.events
				<-s.events
			}
			s.update([]byte(test.update))
			if !turn.poisoned {
				t.Fatal("invalid tool update did not poison turn")
			}
		})
	}
}

func TestToolCacheIsHardBounded(t *testing.T) {
	s, turn := newLiveToolSession(t)
	close(turn.accepted)
	turn.admission = admissionAccepted
	for index := 0; index < maxCachedToolsPerTurn; index++ {
		raw := fmt.Sprintf(`{"sessionId":"native","update":{"sessionUpdate":"tool_call","toolCallId":"id-%d","title":"Tool","kind":"read"}}`, index)
		s.update([]byte(raw))
		event := <-s.events
		if event.Kind != provider.EventToolActivity {
			t.Fatalf("event %d = %s", index, event.Kind)
		}
	}
	s.update([]byte(fmt.Sprintf(`{"sessionId":"native","update":{"sessionUpdate":"tool_call","toolCallId":"id-%d","title":"Tool","kind":"read"}}`, maxCachedToolsPerTurn)))
	if !turn.poisoned {
		t.Fatal("over-limit tool call did not poison turn")
	}
}
