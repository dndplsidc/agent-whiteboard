package cursor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/acp"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/acp/acptest"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
)

func TestPermissionUsesSafeIDsAndAcknowledgedFirstResponseWins(t *testing.T) {
	driver, launcher, root := testDriver(t)
	opened, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	session := opened.(*Session)
	turn := &activePrompt{request: provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("reader")}, accepted: make(chan struct{}), done: make(chan error, 1), result: make(chan promptResult, 1), assistantID: safeID("assistant", turnID), permissionGateOpen: true, tools: make(map[string]cachedTool)}
	session.mu.Lock()
	session.active = turn
	nativeSession := session.native.Ref.Value()
	session.mu.Unlock()
	launcher.mu.Lock()
	child := launcher.children[0]
	launcher.mu.Unlock()
	child.send(t, map[string]any{"jsonrpc": "2.0", "id": 99, "method": "session/request_permission", "params": map[string]any{"sessionId": nativeSession, "toolCall": map[string]any{"toolCallId": "native-tool-secret", "title": "Run command", "kind": "execute"}, "options": []any{map[string]any{"optionId": "native-allow-secret", "name": "Allow once", "kind": "allow_once"}, map[string]any{"optionId": "native-reject-secret", "name": "Reject", "kind": "reject_once"}}}})
	var interaction provider.InteractionRequest
	for index := 0; index < 2; index++ {
		event := <-session.events
		if event.Kind == provider.EventInteractionRequest {
			interaction = *event.Interaction
		}
	}
	if interaction.ID == "" || strings.Contains(interaction.ID, "native") || interaction.Options[0].ID != "allowOnce" || interaction.LocalDeadline == nil {
		t.Fatalf("unsafe or unbounded interaction: %+v", interaction)
	}
	response := provider.InteractionResponse{RequestID: interaction.ID, Kind: interaction.Kind, OptionID: "allowOnce"}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := session.Respond(cancelled, response); err == nil {
		t.Fatal("cancelled not-written response reported success")
	}
	if err := session.Respond(context.Background(), response); err != nil {
		t.Fatal(err)
	}
	var frame map[string]any
	if err := json.Unmarshal(<-child.responses, &frame); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(frame["result"])
	if strings.Count(string(encoded), "native-allow-secret") != 1 || strings.Contains(string(encoded), "native-tool-secret") {
		t.Fatalf("bad native response %s", encoded)
	}
	if err := session.Respond(context.Background(), response); err == nil {
		t.Fatal("second response unexpectedly won")
	}
	select {
	case event := <-session.events:
		t.Fatalf("unexpected event after browser response = %#v", event)
	default:
	}
	_ = session.Shutdown(context.Background())
}

func TestBrowserPermissionDeliveryOutcomesDoNotEmitNativeResolution(t *testing.T) {
	for _, delivery := range []acp.Delivery{acp.Complete, acp.NotWritten, acp.Indeterminate} {
		t.Run(string(delivery), func(t *testing.T) {
			id := safeID("permission", turnID+"\x00tool")
			pending := &pendingPermission{
				request:        provider.InteractionRequest{ID: id, TurnID: turnID, Kind: provider.InteractionCommandApproval, Title: "Permission", Options: []provider.InteractionOption{{ID: "allowOnce", Label: "Allow"}}},
				optionID:       "allowOnce",
				browserClaimed: true,
				published:      true,
				state:          permissionWriting,
				changed:        make(chan struct{}),
			}
			session := &Session{events: make(chan provider.Event, 1), permissions: map[string]*pendingPermission{id: pending}, permissionOutcomes: make(map[string]acp.Delivery)}
			failed := session.recordPermissionOutcomeLocked(id, pending, delivery)
			if failed != (delivery != acp.Complete) {
				t.Fatalf("failed = %v for delivery %s", failed, delivery)
			}
			select {
			case event := <-session.events:
				t.Fatalf("browser-owned %s delivery emitted native resolution = %#v", delivery, event)
			default:
			}
		})
	}
}

func TestNotWrittenBrowserClaimRetainsResolutionOwnershipThroughCancel(t *testing.T) {
	driver, launcher, root := testDriver(t)
	opened, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	session := opened.(*Session)
	turn := &activePrompt{request: provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("reader")}, accepted: make(chan struct{}), done: make(chan error, 1), result: make(chan promptResult, 1), assistantID: safeID("assistant", turnID), permissionGateOpen: true, tools: make(map[string]cachedTool)}
	session.mu.Lock()
	session.active = turn
	nativeSession := session.native.Ref.Value()
	session.mu.Unlock()
	launcher.mu.Lock()
	child := launcher.children[0]
	launcher.mu.Unlock()
	child.send(t, map[string]any{"jsonrpc": "2.0", "id": 100, "method": "session/request_permission", "params": map[string]any{"sessionId": nativeSession, "toolCall": map[string]any{"toolCallId": "native-tool", "title": "Run command", "kind": "execute"}, "options": []any{map[string]any{"optionId": "native-allow", "name": "Allow once", "kind": "allow_once"}}}})
	var interaction provider.InteractionRequest
	for interaction.ID == "" {
		event := <-session.events
		if event.Kind == provider.EventInteractionRequest {
			interaction = *event.Interaction
		}
	}
	response := provider.InteractionResponse{RequestID: interaction.ID, Kind: interaction.Kind, OptionID: "allowOnce"}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := session.Respond(cancelled, response); err == nil {
		t.Fatal("cancelled NotWritten response reported success")
	}
	if err := session.CancelInteraction(context.Background(), interaction.ID); err != nil {
		t.Fatal(err)
	}
	<-child.responses
	select {
	case event := <-session.events:
		t.Fatalf("browser-owned NotWritten claim emitted native resolution after cancellation = %#v", event)
	default:
	}
	_ = session.Shutdown(context.Background())
}

func TestStopClaimsPermissionBeforeBrowserCanRespond(t *testing.T) {
	driver, launcher, root := testDriver(t)
	opened, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	s := opened.(*Session)
	launcher.mu.Lock()
	child := launcher.children[0]
	launcher.mu.Unlock()
	turn := &activePrompt{request: provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("reader")}, accepted: make(chan struct{}), rejected: make(chan struct{}), permissionGateOpen: true, phase: turnRunning}
	s.mu.Lock()
	s.active = turn
	native, settings, presentation := s.native.Ref.Value(), s.settings, s.presentation
	s.mu.Unlock()
	child.send(t, map[string]any{"jsonrpc": "2.0", "id": 101, "method": "session/request_permission", "params": map[string]any{"sessionId": native, "toolCall": map[string]any{"toolCallId": "tool", "title": "Run", "kind": "execute"}, "options": []any{map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"}}}})
	<-s.events
	interaction := <-s.events
	accepted := provider.AcceptedTurn{TurnID: turnID, AcceptedAt: driver.config.Clock.Now().UTC(), Settings: copySettings(settings), Presentation: copyPresentation(presentation)}
	stopped := make(chan error, 1)
	go func() { stopped <- s.Interrupt(context.Background(), accepted) }()
	<-child.responses
	response := provider.InteractionResponse{RequestID: interaction.Interaction.ID, Kind: interaction.Interaction.Kind, OptionID: "allowOnce"}
	if err := s.Respond(context.Background(), response); err == nil {
		t.Fatal("browser response won after Stop claimed admission")
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	_ = s.Shutdown(context.Background())
}

func TestInterruptCancelsPermissionBeforeCancelNotification(t *testing.T) {
	driver, launcher, root := testDriver(t)
	opened, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	session := opened.(*Session)
	turn := &activePrompt{request: provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("reader")}, accepted: make(chan struct{}), done: make(chan error, 1), result: make(chan promptResult, 1), assistantID: safeID("assistant", turnID), permissionGateOpen: true, tools: make(map[string]cachedTool)}
	session.mu.Lock()
	session.active = turn
	nativeSession := session.native.Ref.Value()
	settings, presentation := session.settings, session.presentation
	session.mu.Unlock()
	launcher.mu.Lock()
	child := launcher.children[0]
	launcher.mu.Unlock()
	child.send(t, map[string]any{"jsonrpc": "2.0", "id": 100, "method": "session/request_permission", "params": map[string]any{"sessionId": nativeSession, "toolCall": map[string]any{"toolCallId": "tool", "title": "Run command", "kind": "execute"}, "options": []any{map[string]any{"optionId": "allow", "name": "Allow once", "kind": "allow_once"}}}})
	<-session.events // user acceptance
	<-session.events // interaction
	accepted := provider.AcceptedTurn{TurnID: turnID, AcceptedAt: driver.config.Clock.Now().UTC(), Settings: copySettings(settings), Presentation: copyPresentation(presentation)}
	if err = session.Interrupt(context.Background(), accepted); err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err = json.Unmarshal(<-child.responses, &response); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(response["result"])
	if !strings.Contains(string(encoded), "cancelled") {
		t.Fatalf("permission outcome = %s", encoded)
	}
	// A completed write does not imply that the scripted child goroutine has
	// parsed it. A following request/response is a deterministic FIFO barrier
	// proving that session/cancel was observed before this assertion.
	var barrier json.RawMessage
	if _, err = session.rt.client.Call(context.Background(), "session/list", map[string]any{}, &barrier); err != nil {
		t.Fatal(err)
	}
	child.mu.Lock()
	observed := append([]string(nil), child.observed...)
	child.mu.Unlock()
	cancelIndex, barrierIndex := slices.Index(observed, "session/cancel"), slices.Index(observed, "session/list")
	if cancelIndex < 1 || observed[cancelIndex-1] != "response" || barrierIndex <= cancelIndex {
		t.Fatalf("write order = %v", observed)
	}
	_ = session.Shutdown(context.Background())
}

func TestResponderExpiryResolvesOnceAndIndeterminateFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name    string
		partial bool
	}{{"complete", false}, {"indeterminate", true}} {
		t.Run(test.name, func(t *testing.T) {
			driver, _, root := testDriver(t)
			process := acptest.NewProcess()
			if test.partial {
				process.Stdin.CompleteBlockedWriteOnClose(1, io.ErrClosedPipe)
			}
			var s *Session
			client, err := acp.New(process, acp.Options{HandlerTimeout: 5 * time.Millisecond, GracePeriod: time.Millisecond, TerminatePeriod: time.Millisecond, FinalPeriod: 10 * time.Millisecond, DrainPeriod: 10 * time.Millisecond, MaxHandlerConcurrency: 1, Handler: func(ctx context.Context, request acp.Request) { s.handle(ctx, request) }})
			if err != nil {
				t.Fatal(err)
			}
			s = newSession(driver, &runtime{client: client}, root)
			go func() { <-client.Done(); s.transportEnded() }()
			if err := s.finishOpen(openResult{SessionID: "native", ConfigOptions: testOptions("model-a")}, false); err != nil {
				t.Fatal(err)
			}
			turn := &activePrompt{request: provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("reader")}, accepted: make(chan struct{}), rejected: make(chan struct{}), permissionGateOpen: true, phase: turnRunning}
			s.mu.Lock()
			s.active = turn
			s.mu.Unlock()
			go func() { <-process.TerminateCalled; process.Complete(nil) }()
			frame := `{"jsonrpc":"2.0","id":7,"method":"session/request_permission","params":{"sessionId":"native","toolCall":{"toolCallId":"tool","title":"Run","kind":"execute"},"options":[{"optionId":"allow","name":"Allow","kind":"allow_once"}]}}` + "\n"
			if _, err := process.OutputWriter.Write([]byte(frame)); err != nil {
				t.Fatal(err)
			}
			<-s.events
			request := <-s.events
			if request.Interaction.LocalDeadline == nil {
				t.Fatal("expiry deadline missing")
			}
			resolved := <-s.events
			if resolved.Kind != provider.EventInteractionResolved || resolved.Resolution.RequestID != request.Interaction.ID {
				t.Fatalf("resolution = %#v", resolved)
			}
			select {
			case duplicate := <-s.events:
				if duplicate.Kind == provider.EventInteractionResolved {
					t.Fatal("duplicate resolution")
				}
			default:
			}
			if test.partial {
				<-client.Done()
				for event := range s.events {
					if event.Kind == provider.EventCompletion || event.Kind == provider.EventInterruption || event.Kind == provider.EventTerminalFailure {
						t.Fatalf("Indeterminate expiry emitted terminal %#v", event)
					}
				}
			} else {
				_ = client.Shutdown(context.Background())
			}
		})
	}
}

func TestSequentialPermissionTombstoneBoundAndTerminalCleanup(t *testing.T) {
	driver, launcher, root := testDriver(t)
	opened, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	s := opened.(*Session)
	launcher.mu.Lock()
	child := launcher.children[0]
	launcher.mu.Unlock()
	turn := &activePrompt{request: provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("reader")}, accepted: make(chan struct{}), rejected: make(chan struct{}), permissionGateOpen: true, phase: turnRunning}
	s.mu.Lock()
	s.active = turn
	native := s.native.Ref.Value()
	s.mu.Unlock()
	for index := 0; index < maxPermissionsPerTurn; index++ {
		child.send(t, map[string]any{"jsonrpc": "2.0", "id": 600 + index, "method": "session/request_permission", "params": map[string]any{"sessionId": native, "toolCall": map[string]any{"toolCallId": fmt.Sprintf("tool-%d", index), "title": "Run", "kind": "execute"}, "options": []any{map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"}}}})
		var request provider.Event
		for request.Kind != provider.EventInteractionRequest {
			request = <-s.events
		}
		if err := s.CancelInteraction(context.Background(), request.Interaction.ID); err != nil {
			t.Fatal(err)
		}
		<-child.responses
		resolved := <-s.events
		if resolved.Kind != provider.EventInteractionResolved {
			t.Fatalf("resolution %d = %s", index, resolved.Kind)
		}
	}
	s.mu.Lock()
	tombstones := len(s.permissionOutcomes)
	s.mu.Unlock()
	if tombstones != maxPermissionsPerTurn {
		t.Fatalf("tombstones = %d", tombstones)
	}
	child.send(t, map[string]any{"jsonrpc": "2.0", "id": 999, "method": "session/request_permission", "params": map[string]any{"sessionId": native, "toolCall": map[string]any{"toolCallId": "over-limit", "title": "Run", "kind": "execute"}, "options": []any{map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"}}}})
	<-child.responses
	<-child.done
	for range s.events {
	}
	s.mu.Lock()
	live, outcomes, order := len(s.permissions), len(s.permissionOutcomes), len(s.permissionOrder)
	s.mu.Unlock()
	if live != 0 || outcomes != 0 || order != 0 {
		t.Fatalf("terminal permission state live=%d outcomes=%d order=%d", live, outcomes, order)
	}
	_ = s.Shutdown(context.Background())
}

func TestInvalidPermissionFramesQuarantineFollowingValidTraffic(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]any
	}{
		{"malformed", map[string]any{"sessionId": "native", "toolCall": map[string]any{"toolCallId": "", "title": "Run", "kind": "execute"}, "options": []any{map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"}}}},
		{"wrong-session", map[string]any{"sessionId": "wrong", "toolCall": map[string]any{"toolCallId": "tool", "title": "Run", "kind": "execute"}, "options": []any{map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"}}}},
		{"duplicate-options", map[string]any{"sessionId": "native", "toolCall": map[string]any{"toolCallId": "tool", "title": "Run", "kind": "execute"}, "options": []any{map[string]any{"optionId": "same", "name": "Allow", "kind": "allow_once"}, map[string]any{"optionId": "same", "name": "Reject", "kind": "reject_once"}}}},
	}
	for _, test := range cases {
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
			turn := &activePrompt{request: provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("reader")}, accepted: make(chan struct{}), rejected: make(chan struct{}), permissionGateOpen: true, phase: turnRunning}
			s.mu.Lock()
			s.active = turn
			native := s.native.Ref.Value()
			if test.name != "wrong-session" {
				test.params["sessionId"] = native
			}
			child.send(t, map[string]any{"jsonrpc": "2.0", "id": 501, "method": "session/request_permission", "params": test.params})
			child.send(t, map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": native, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "late"}}}})
			s.mu.Unlock()
			<-child.responses
			<-child.done
			if !channelClosed(turn.rejected) || channelClosed(turn.accepted) {
				t.Fatalf("admission accepted=%v rejected=%v", channelClosed(turn.accepted), channelClosed(turn.rejected))
			}
			s.finishPrompt(turn, "end_turn")
			s.mu.Lock()
			active := s.active
			s.mu.Unlock()
			if active != nil {
				t.Fatal("quarantined prompt remained active")
			}
		})
	}
}

func TestUnknownInboundRequestReceivesMethodNotFound(t *testing.T) {
	driver, launcher, root := testDriver(t)
	opened, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	session := opened.(*Session)
	launcher.mu.Lock()
	child := launcher.children[0]
	launcher.mu.Unlock()
	child.send(t, map[string]any{"jsonrpc": "2.0", "id": "extension-secret", "method": "cursor/private", "params": map[string]any{}})
	var frame struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(<-child.responses, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Error.Code != -32601 {
		t.Fatalf("error code = %d", frame.Error.Code)
	}
	_ = session.Shutdown(context.Background())
}
