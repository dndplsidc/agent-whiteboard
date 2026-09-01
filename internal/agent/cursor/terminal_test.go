package cursor

import (
	"context"
	"sync"
	"testing"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/acp"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
)

func startBlockedTurn(t *testing.T, session *Session, child *scriptChild, request provider.TurnRequest) (<-chan struct{}, chan string, <-chan submitResult) {
	t.Helper()
	child.mu.Lock()
	child.promptStarted = make(chan struct{})
	child.promptRelease = make(chan string, 1)
	started, release := child.promptStarted, child.promptRelease
	child.mu.Unlock()
	result := make(chan submitResult, 1)
	go func() {
		accepted, err := session.Submit(context.Background(), request)
		result <- submitResult{accepted, err}
	}()
	return started, release, result
}

type submitResult struct {
	accepted provider.AcceptedTurn
	err      error
}

func acceptBlockedTurn(t *testing.T, session *Session, child *scriptChild, started <-chan struct{}, result <-chan submitResult, text string) provider.AcceptedTurn {
	t.Helper()
	<-started
	child.send(t, map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": session.NativeSession().Ref.Value(), "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}}}})
	got := <-result
	if got.err != nil {
		t.Fatal(got.err)
	}
	return got.accepted
}

func drainTurn(events <-chan provider.Event, terminal provider.EventKind) {
	for event := range events {
		if event.Kind == terminal {
			return
		}
	}
}

func TestStopJoinIsPerTurnAndConcurrentStopWritesOnce(t *testing.T) {
	driver, launcher, root := testDriver(t)
	opened, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	session := opened.(*Session)
	launcher.mu.Lock()
	child := launcher.children[0]
	launcher.mu.Unlock()

	makeTurn := func(id, message string) provider.AcceptedTurn {
		turn := &activePrompt{request: provider.TurnRequest{TurnID: id, MessageID: message, Content: provider.TextMessage("reader")}, accepted: make(chan struct{}), rejected: make(chan struct{}), permissionGateOpen: true, phase: turnRunning}
		close(turn.accepted)
		turn.admission = admissionAccepted
		session.mu.Lock()
		session.active = turn
		settings, presentation := session.settings, session.presentation
		session.mu.Unlock()
		return provider.AcceptedTurn{TurnID: id, AcceptedAt: driver.config.Clock.Now().UTC(), Settings: copySettings(settings), Presentation: copyPresentation(presentation)}
	}
	accepted := makeTurn(turnID, messageID)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() { <-start; results <- session.Interrupt(context.Background(), accepted) }()
	}
	close(start)
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	if err := <-results; err != nil {
		t.Fatal(err)
	}

	second := makeTurn(safeID("second-turn", turnID), safeID("second-message", messageID))
	if err := session.Interrupt(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	var barrier listResult
	if _, err := session.rt.client.Call(context.Background(), "session/list", map[string]any{}, &barrier); err != nil {
		t.Fatal(err)
	}
	child.mu.Lock()
	methods := append([]string(nil), child.methods...)
	child.mu.Unlock()
	cancels := 0
	for _, method := range methods {
		if method == "session/cancel" {
			cancels++
		}
	}
	if cancels != 2 {
		t.Fatalf("session/cancel writes = %d (%v)", cancels, methods)
	}
	_ = session.Shutdown(context.Background())
}

func TestShutdownJoinsInterruptInstalledDuringStopClaimWindow(t *testing.T) {
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
	child.send(t, map[string]any{"jsonrpc": "2.0", "id": 333, "method": "session/request_permission", "params": map[string]any{"sessionId": native, "toolCall": map[string]any{"toolCallId": "tool", "title": "Run", "kind": "execute"}, "options": []any{map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"}}}})
	<-s.events
	<-s.events
	writeGate, writeStarted := make(chan struct{}), make(chan struct{})
	child.mu.Lock()
	child.inputGate, child.inputStarted = writeGate, writeStarted
	child.mu.Unlock()
	shutdownObserved, releaseShutdown := make(chan struct{}), make(chan struct{})
	shutdownClaimed := make(chan bool, 1)
	s.mu.Lock()
	s.beforeShutdownStop = func() { close(shutdownObserved); <-releaseShutdown }
	s.afterShutdownStopClaim = func(owner bool) { shutdownClaimed <- owner }
	s.mu.Unlock()
	shutdownResult := make(chan error, 1)
	go func() { shutdownResult <- s.Shutdown(context.Background()) }()
	<-shutdownObserved
	accepted := provider.AcceptedTurn{TurnID: turnID, AcceptedAt: driver.config.Clock.Now().UTC(), Settings: copySettings(settings), Presentation: copyPresentation(presentation)}
	interruptResult := make(chan error, 1)
	go func() { interruptResult <- s.Interrupt(context.Background(), accepted) }()
	<-writeStarted
	close(releaseShutdown)
	if owner := <-shutdownClaimed; owner {
		t.Fatal("Shutdown replaced Interrupt Stop ownership")
	}
	select {
	case <-child.done:
		t.Fatal("runtime closed before Stop completed")
	default:
	}
	select {
	case err := <-shutdownResult:
		t.Fatalf("Shutdown returned before Stop completed: %v", err)
	default:
	}
	close(writeGate)
	if err := <-interruptResult; err != nil {
		t.Fatal(err)
	}
	if err := <-shutdownResult; err != nil {
		t.Fatal(err)
	}
	<-child.done
	child.mu.Lock()
	observed := append([]string(nil), child.observed...)
	child.mu.Unlock()
	responses, cancels := 0, 0
	for _, item := range observed {
		if item == "response" {
			responses++
		}
		if item == "session/cancel" {
			cancels++
		}
	}
	if responses != 1 || cancels != 1 {
		t.Fatalf("settlements=%d cancels=%d observed=%v", responses, cancels, observed)
	}
}

func TestPriorNonCompletePermissionOutcomeBlocksStopAndPromptTerminal(t *testing.T) {
	for _, operation := range []string{"stop", "prompt"} {
		t.Run(operation, func(t *testing.T) {
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
			close(turn.accepted)
			turn.admission = admissionAccepted
			s.mu.Lock()
			s.active = turn
			s.permissionOutcomes["settled"] = acp.Indeterminate
			settings, presentation := s.settings, s.presentation
			s.mu.Unlock()
			if operation == "stop" {
				accepted := provider.AcceptedTurn{TurnID: turnID, AcceptedAt: driver.config.Clock.Now().UTC(), Settings: copySettings(settings), Presentation: copyPresentation(presentation)}
				if err := s.Interrupt(context.Background(), accepted); err == nil {
					t.Fatal("Stop ignored prior Indeterminate")
				}
			} else {
				s.finishPrompt(turn, "end_turn")
			}
			<-child.done
			terminals := 0
			for event := range s.events {
				if event.Kind == provider.EventCompletion || event.Kind == provider.EventInterruption || event.Kind == provider.EventTerminalFailure {
					terminals++
				}
			}
			if terminals != 0 {
				t.Fatalf("unsettled permission emitted %d terminal events", terminals)
			}
			child.mu.Lock()
			methods := append([]string(nil), child.methods...)
			child.mu.Unlock()
			for _, method := range methods {
				if method == "session/cancel" {
					t.Fatal("Stop replayed after Indeterminate")
				}
			}
		})
	}
}

func TestEventOverflowPreAndPostAcceptanceClosesTransportAndEvents(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "pre", true: "post"}[accepted], func(t *testing.T) {
			driver, launcher, root := testDriver(t)
			opened, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
			if err != nil {
				t.Fatal(err)
			}
			s := opened.(*Session)
			launcher.mu.Lock()
			child := launcher.children[0]
			launcher.mu.Unlock()
			turn := &activePrompt{request: provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("reader")}, accepted: make(chan struct{}), rejected: make(chan struct{}), permissionGateOpen: true, phase: turnRunning, tools: map[string]cachedTool{}}
			if accepted {
				close(turn.accepted)
				turn.admission = admissionAccepted
			}
			s.mu.Lock()
			s.active = turn
			for len(s.events) < cap(s.events) {
				s.events <- provider.NewActivityEvent(turnID, provider.ActivityStatus, "full")
			}
			s.publishLocked(provider.NewActivityEvent(turnID, provider.ActivityStatus, "overflow"))
			s.mu.Unlock()
			<-child.done
			for range s.events {
			}
			s.mu.Lock()
			closed := s.eventsClosed && s.phase != sessionRunning
			active := s.active
			s.mu.Unlock()
			if !closed || active != nil {
				t.Fatalf("closed=%v active=%p", closed, active)
			}
		})
	}
}

func TestSubmitContextCancellationDoesNotWedgeNativeTurn(t *testing.T) {
	driver, launcher, root := testDriver(t)
	opened, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	s := opened.(*Session)
	launcher.mu.Lock()
	child := launcher.children[0]
	launcher.mu.Unlock()
	child.mu.Lock()
	child.promptStarted, child.promptRelease = make(chan struct{}), make(chan string, 1)
	started, release := child.promptStarted, child.promptRelease
	child.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, submitErr := s.Submit(ctx, provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("reader")})
		result <- submitErr
	}()
	<-started
	cancel()
	if err := <-result; err == nil {
		t.Fatal("cancelled Submit unexpectedly established acceptance")
	}
	release <- "end_turn"
	drainTurn(s.events, provider.EventCompletion)
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	if active != nil {
		t.Fatal("context-cancelled turn remained active")
	}
	_ = s.Shutdown(context.Background())
}

func TestPendingSubmitTransportExitClearsActiveAndClosesEvents(t *testing.T) {
	driver, launcher, root := testDriver(t)
	opened, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	s := opened.(*Session)
	launcher.mu.Lock()
	child := launcher.children[0]
	launcher.mu.Unlock()
	child.mu.Lock()
	child.promptStarted, child.promptRelease = make(chan struct{}), make(chan string, 1)
	started := child.promptStarted
	child.mu.Unlock()
	result := make(chan error, 1)
	go func() {
		_, submitErr := s.Submit(context.Background(), provider.TurnRequest{TurnID: turnID, MessageID: messageID, Content: provider.TextMessage("reader")})
		result <- submitErr
	}()
	<-started
	child.stop()
	if err := <-result; err == nil {
		t.Fatal("transport exit reported accepted")
	}
	for range s.events {
	}
	s.mu.Lock()
	active, closed := s.active, s.eventsClosed
	s.mu.Unlock()
	if active != nil || !closed {
		t.Fatalf("active=%p closed=%v", active, closed)
	}
}

func TestInvalidStopReasonPreAndPostAcceptanceCannotLaterComplete(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "pre", true: "post"}[accepted], func(t *testing.T) {
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
			if accepted {
				close(turn.accepted)
				turn.admission = admissionAccepted
			}
			s.mu.Lock()
			s.active = turn
			s.mu.Unlock()
			s.finishPrompt(turn, "native-private-stop")
			<-child.done
			terminalFailures, completions := 0, 0
			for event := range s.events {
				if event.Kind == provider.EventTerminalFailure {
					terminalFailures++
				}
				if event.Kind == provider.EventCompletion {
					completions++
				}
			}
			if completions != 0 || terminalFailures != map[bool]int{false: 0, true: 1}[accepted] {
				t.Fatalf("failure=%d completion=%d", terminalFailures, completions)
			}
			if !accepted && !channelClosed(turn.rejected) {
				t.Fatal("pre-acceptance invalid stop did not reject")
			}
		})
	}
}

func TestTransportExitWithNotWrittenPermissionResolvesWithoutTurnTerminal(t *testing.T) {
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
	child.send(t, map[string]any{"jsonrpc": "2.0", "id": 77, "method": "session/request_permission", "params": map[string]any{"sessionId": native, "toolCall": map[string]any{"toolCallId": "tool", "title": "Run", "kind": "execute"}, "options": []any{map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"}}}})
	<-s.events
	request := <-s.events
	if request.Kind != provider.EventInteractionRequest {
		t.Fatalf("request event = %s", request.Kind)
	}
	child.stop()
	resolved := <-s.events
	if resolved.Kind != provider.EventInteractionResolved || resolved.Resolution.RequestID != request.Interaction.ID {
		t.Fatalf("resolution = %#v", resolved)
	}
	for event := range s.events {
		if event.Kind == provider.EventCompletion || event.Kind == provider.EventInterruption || event.Kind == provider.EventTerminalFailure {
			t.Fatalf("unsettled transport emitted terminal %#v", event)
		}
	}
}

func TestShutdownWinsTransportExitReason(t *testing.T) {
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
	close(turn.accepted)
	turn.admission = admissionAccepted
	s.mu.Lock()
	s.active = turn
	s.mu.Unlock()
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-child.done
	count := 0
	for event := range s.events {
		if event.Kind == provider.EventInterruption {
			count++
			if event.Interruption != provider.InterruptionShutdown {
				t.Fatalf("reason = %s", event.Interruption)
			}
		}
	}
	if count != 1 {
		t.Fatalf("shutdown interruptions = %d", count)
	}
}

func TestShutdownPermissionWriteFailureStillClosesChild(t *testing.T) {
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
	child.send(t, map[string]any{"jsonrpc": "2.0", "id": 88, "method": "session/request_permission", "params": map[string]any{"sessionId": native, "toolCall": map[string]any{"toolCallId": "tool", "title": "Run", "kind": "execute"}, "options": []any{map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"}}}})
	<-s.events
	<-s.events
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Shutdown(cancelled); err == nil {
		t.Fatal("cancelled shutdown unexpectedly succeeded")
	}
	<-child.done
}

func TestConcurrentShutdownJoinsAndClosesChild(t *testing.T) {
	driver, launcher, root := testDriver(t)
	opened, err := driver.Create(context.Background(), provider.CreateRequest{Provider: provider.NameCursor, Access: provider.AccessConfigured, Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	s := opened.(*Session)
	launcher.mu.Lock()
	child := launcher.children[0]
	launcher.mu.Unlock()
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; results <- s.Shutdown(context.Background()) }()
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	<-child.done
}
