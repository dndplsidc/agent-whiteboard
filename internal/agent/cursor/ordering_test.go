package cursor

import (
	"context"
	"testing"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
)

func TestWireOrderedUpdateHandlersPrecedePromptResult(t *testing.T) {
	for _, test := range []struct {
		name         string
		update       map[string]any
		wantAccepted bool
	}{
		{name: "valid", update: map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "accepted"}}, wantAccepted: true},
		{name: "malformed", update: map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": ""}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			driver, launcher, root := testDriver(t)
			opened, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
			if err != nil {
				t.Fatal(err)
			}
			s := opened.(*Session)
			launcher.mu.Lock()
			child := launcher.children[0]
			launcher.mu.Unlock()
			started, release, submitted := startBlockedTurn(t, s, child, provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("reader")})
			<-started
			nativeID := s.NativeSession().Ref.Value()
			s.mu.Lock()
			child.send(t, map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": nativeID, "update": test.update}})
			release <- "end_turn"
			s.mu.Unlock()
			result := <-submitted
			if (result.err == nil) != test.wantAccepted {
				t.Fatalf("accepted=%v err=%v", result.err == nil, result.err)
			}
			if test.wantAccepted {
				drainTurn(s.events, provider.EventCompletion)
			} else {
				<-child.done
			}
			_ = s.Shutdown(context.Background())
		})
	}
}

func TestWireOrderedPermissionRequestPrecedesPromptResult(t *testing.T) {
	driver, launcher, root := testDriver(t)
	opened, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	s := opened.(*Session)
	launcher.mu.Lock()
	child := launcher.children[0]
	launcher.mu.Unlock()
	started, release, submitted := startBlockedTurn(t, s, child, provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("reader")})
	<-started
	nativeID := s.NativeSession().Ref.Value()
	s.mu.Lock()
	child.send(t, map[string]any{"jsonrpc": "2.0", "id": 991, "method": "session/request_permission", "params": map[string]any{"sessionId": nativeID, "toolCall": map[string]any{"toolCallId": "tool", "title": "Run", "kind": "execute"}, "options": []any{map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"}}}})
	release <- "end_turn"
	s.mu.Unlock()
	result := <-submitted
	if result.err != nil {
		t.Fatal(result.err)
	}
	kinds := []provider.EventKind{}
	for event := range s.events {
		kinds = append(kinds, event.Kind)
		if event.Kind == provider.EventCompletion {
			break
		}
	}
	wantInteraction, wantResolved := false, false
	for _, kind := range kinds {
		if kind == provider.EventInteractionRequest {
			wantInteraction = true
		}
		if kind == provider.EventInteractionResolved {
			wantResolved = true
		}
	}
	if !wantInteraction || !wantResolved {
		t.Fatalf("events = %v", kinds)
	}
	_ = s.Shutdown(context.Background())
}
