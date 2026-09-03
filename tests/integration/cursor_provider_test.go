package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/attachment"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/protocol"
	"github.com/dndplsidc/agent-whiteboard/internal/app"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

const (
	cursorSource  = "# Fixture board\nHermetic semantic source."
	cursorCreator = "hermetic creator context"
)

type cursorHarness struct {
	service                                                 *app.AgentService
	cancel                                                  context.CancelFunc
	done                                                    chan error
	origin, baseURL, home, statePath, evidencePath, control string
}

func newCursorHarness(t *testing.T, scenario string) *cursorHarness {
	t.Helper()
	root := t.TempDir()
	origin := "http://127.0.0.1:43191"
	configPath := filepath.Join(root, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("version: 1\nagent:\n  trusted_origins:\n    - "+origin+"\n"), 0o600))
	control := filepath.Join(root, "control")
	require.NoError(t, os.Mkdir(control, 0o700))
	h := &cursorHarness{origin: origin, statePath: filepath.Join(root, "fixture-state.json"), evidencePath: filepath.Join(root, "evidence.jsonl"), control: control, done: make(chan error, 1)}
	home := filepath.Join(root, "home")
	h.home = home
	require.NoError(t, os.Mkdir(home, 0o700))
	service, err := app.NewAgentService(app.AgentServiceConfig{
		ConfigPath: configPath, Home: home, Port: 0,
		CursorExecutable:  scriptedCursorACPPath,
		CursorEnvironment: []string{"AWB_CURSOR_STATE=" + h.statePath, "AWB_CURSOR_EVIDENCE=" + h.evidencePath, "AWB_CURSOR_CONTROL=" + control, "AWB_CURSOR_SCENARIO=" + scenario},
		IdleTimeout:       20 * time.Millisecond, ShutdownTimeout: 2 * time.Second,
	})
	require.NoError(t, err)
	h.service = service
	h.baseURL = "http://" + service.Host()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- service.ListenAndServe(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-h.done:
			require.NoError(t, err)
		case <-time.After(processTimeout):
			t.Errorf("Cursor service did not stop")
		}
	})
	return h
}

type cursorSocket struct {
	ws                       *websocket.Conn
	clientID, conversationID string
	next                     int
}

func (h *cursorHarness) connect(t *testing.T) *cursorSocket {
	t.Helper()
	return h.connectResource(t, cursorID('C'))
}

func (h *cursorHarness) connectResource(t *testing.T, resourceID string) *cursorSocket {
	t.Helper()
	dialer := websocket.Dialer{Subprotocols: []string{protocol.WebSocketSubprotocol}, HandshakeTimeout: processTimeout}
	u := url.URL{Scheme: "ws", Host: h.service.Host(), Path: protocol.ConnectPath}
	header := http.Header{"Origin": []string{h.origin}}
	ws, response, err := dialer.Dial(u.String(), header)
	if response != nil {
		defer response.Body.Close()
	}
	require.NoError(t, err)
	s := &cursorSocket{ws: ws, clientID: cursorID('B'), next: 1}
	created := time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC)
	settings := protocol.ExecutionSettings{Model: "cursor-large", Effort: "default", Speed: protocol.SpeedStandard}
	command := protocol.Command{APIVersion: protocol.APIVersion, CommandID: cursorID('A'), ClientID: s.clientID, Type: protocol.CommandConnect, Payload: protocol.ConnectPayload{Provider: protocol.ProviderCursor, Resource: protocol.Resource{Kind: protocol.ResourceMarkdown, ID: resourceID, CreatedAt: created, UpdatedAt: created}, ContextDigest: agent.CalculateContextDigest([]byte(cursorSource), []byte(cursorCreator)), Settings: &settings}}
	s.write(t, command)
	for {
		e := s.read(t)
		if id, _ := e["conversation_id"].(string); id != "" {
			s.conversationID = id
		}
		if e["type"] == string(protocol.EventSnapshot) {
			payload := e["payload"].(map[string]any)
			if payload["lifecycle"] == string(protocol.LifecycleReady) || payload["lifecycle"] == string(protocol.LifecycleUnavailable) {
				return s
			}
		}
	}
}
func cursorID(ch byte) string { return strings.Repeat(string(ch), 32) }
func (s *cursorSocket) write(t *testing.T, c protocol.Command) {
	t.Helper()
	require.NoError(t, s.ws.WriteJSON(c))
}
func (s *cursorSocket) command(t *testing.T, typ protocol.CommandType, payload protocol.CommandPayload) string {
	t.Helper()
	id := fmt.Sprintf("%032d", s.next)
	s.next++
	conversation := s.conversationID
	s.write(t, protocol.Command{APIVersion: protocol.APIVersion, CommandID: id, ClientID: s.clientID, ConversationID: &conversation, Type: typ, Payload: payload})
	return id
}
func (s *cursorSocket) read(t *testing.T) map[string]any {
	t.Helper()
	require.NoError(t, s.ws.SetReadDeadline(time.Now().Add(processTimeout)))
	var v map[string]any
	require.NoError(t, s.ws.ReadJSON(&v))
	return v
}
func (s *cursorSocket) waitType(t *testing.T, typ protocol.EventType) map[string]any {
	t.Helper()
	for {
		v := s.read(t)
		if v["type"] == string(typ) {
			return v
		}
	}
}
func (s *cursorSocket) waitResult(t *testing.T, id string) map[string]any {
	t.Helper()
	for {
		v := s.read(t)
		if v["type"] == string(protocol.EventCommandResult) {
			p := v["payload"].(map[string]any)
			if p["command_id"] == id {
				return p
			}
		}
	}
}

func TestCursorReadinessClassificationsThroughAppAndRealSubprocess(t *testing.T) {
	for _, tc := range []struct {
		name, scenario string
		ready          bool
	}{
		{"authentication required", "auth_list", false},
		{"protocol mismatch", "protocol", false},
		{"missing load capability", "missing_load", false},
		{"missing list capability", "missing_list", false},
		{"ready initialize and list", "ready", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCursorHarness(t, tc.scenario)
			if tc.ready {
				s := h.connect(t)
				defer s.ws.Close()
			} else {
				h.requireConnectUnavailable(t)
			}
			evidence := h.waitEvidence(t, "initialize")
			if tc.scenario == "auth_list" {
				require.Contains(t, evidence, "session/list")
			} else if !tc.ready {
				require.NotContains(t, evidence, "session/list")
			}
		})
	}
}

func (h *cursorHarness) requireConnectUnavailable(t *testing.T) {
	t.Helper()
	dialer := websocket.Dialer{Subprotocols: []string{protocol.WebSocketSubprotocol}, HandshakeTimeout: processTimeout}
	u := url.URL{Scheme: "ws", Host: h.service.Host(), Path: protocol.ConnectPath}
	ws, _, err := dialer.Dial(u.String(), http.Header{"Origin": []string{h.origin}})
	require.NoError(t, err)
	defer ws.Close()
	created := time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC)
	require.NoError(t, ws.WriteJSON(protocol.Command{APIVersion: protocol.APIVersion, CommandID: cursorID('A'), ClientID: cursorID('B'), Type: protocol.CommandConnect, Payload: protocol.ConnectPayload{Provider: protocol.ProviderCursor, Resource: protocol.Resource{Kind: protocol.ResourceMarkdown, ID: cursorID('C'), CreatedAt: created, UpdatedAt: created}, ContextDigest: strings.Repeat("0", 64)}}))
	require.NoError(t, ws.SetReadDeadline(time.Now().Add(processTimeout)))
	_, _, err = ws.ReadMessage()
	require.Error(t, err)
}

func TestCursorRealComponentWorkflowPromptModelAndNewHandoff(t *testing.T) {
	h := newCursorHarness(t, "ready")
	s := h.connect(t)
	defer s.ws.Close()
	require.NotEmpty(t, s.conversationID)
	// Readiness is restricted to initialize and list; conversation creation owns a different child.
	evidence := h.waitEvidence(t, "session/new.semantic")
	require.Contains(t, evidence, "session/list")

	created := time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC)
	source := cursorSource
	creator := cursorCreator
	digest := agent.CalculateContextDigest([]byte(source), []byte(creator))
	settings := protocol.ExecutionSettings{Model: "cursor-large", Effort: "default", Speed: protocol.SpeedStandard}
	turnID, messageID := cursorID('D'), cursorID('E')
	contextPayload := &protocol.PageContext{Revision: protocol.ContextInitial, Source: source, CreatorContext: creator, Title: "Fixture", URL: h.origin + "/fixture", Resource: protocol.Resource{Kind: protocol.ResourceMarkdown, ID: cursorID('C'), CreatedAt: created, UpdatedAt: created}, Digest: digest}
	submit := s.command(t, protocol.CommandSubmit, protocol.SubmitPayload{TurnID: turnID, MessageID: messageID, Content: protocol.MessageContent{Parts: []protocol.MessagePart{{Type: protocol.MessagePartText, Text: "reader question"}}}, Context: contextPayload, Settings: &settings})
	seenTool, submitSucceeded, completed := false, false, false
	for !completed {
		e := s.read(t)
		switch e["type"] {
		case string(protocol.EventToolActivity):
			seenTool = true
		case string(protocol.EventCommandResult):
			p := e["payload"].(map[string]any)
			if p["command_id"] == submit {
				require.Equal(t, string(protocol.CommandSucceeded), p["status"], "submit rejected: %#v", p)
				submitSucceeded = true
			}
		case string(protocol.EventCompletion):
			require.Equal(t, turnID, e["payload"].(map[string]any)["turn_id"])
			completed = true
		}
	}
	require.True(t, seenTool)
	if !submitSucceeded {
		submitResult := s.waitResult(t, submit)
		require.Equal(t, string(protocol.CommandSucceeded), submitResult["status"])
	}

	// New crosses the broker archive handoff boundary and creates another native
	// session. The v5 transport intentionally retires this attachment as identity
	// changes; archive list/restore/delete capability ordering is exhaustively
	// asserted by the provider-neutral broker Cursor actor integration suite.
	s.command(t, protocol.CommandNew, protocol.NewPayload{Settings: &settings})
	require.Eventually(t, func() bool {
		b, _ := os.ReadFile(h.evidencePath)
		return strings.Count(string(b), "session/new.semantic") >= 2
	}, processTimeout, pollInterval)

	evidence = h.waitEvidence(t, "session/prompt.semantic")
	require.NotContains(t, evidence, source)
	require.NotContains(t, evidence, creator)
	require.NotContains(t, evidence, "fixture-")
	require.Contains(t, evidence, `"model_option":"cursor-large"`)
	require.Contains(t, evidence, `"provider_envelope_valid":true`)
	require.NotContains(t, evidence, turnID)
}

func TestCursorPermissionResponseThroughAppWebSocket(t *testing.T) {
	h := newCursorHarness(t, "permission")
	s := h.connect(t)
	defer s.ws.Close()
	turnID := cursorID('K')
	submitID := s.command(t, protocol.CommandSubmit, cursorSubmitPayload(h, cursorID('C'), turnID, cursorID('L'), "permission question", nil))
	var requestID, optionID string
	ordering := []string{}
	submitAccepted, responding := false, false
	for requestID == "" || !submitAccepted || !responding {
		event := s.read(t)
		ordering = append(ordering, fmt.Sprint(event["type"]))
		switch event["type"] {
		case string(protocol.EventInteractionRequest):
			payload := event["payload"].(map[string]any)
			requestID = payload["request_id"].(string)
			require.Equal(t, string(protocol.InteractionCommandApproval), payload["kind"])
			options := payload["options"].([]any)
			require.Len(t, options, 2)
			optionID = options[1].(map[string]any)["id"].(string)
			require.Equal(t, "rejectOnce", optionID)
		case string(protocol.EventLifecycle):
			payload := event["payload"].(map[string]any)
			responding = payload["state"] == string(protocol.LifecycleResponding)
		case string(protocol.EventCommandResult):
			payload := event["payload"].(map[string]any)
			if payload["command_id"] == submitID {
				require.Equal(t, string(protocol.CommandSucceeded), payload["status"])
				submitAccepted = true
			}
		}
	}
	responseID := s.command(t, protocol.CommandInteractionRespond, protocol.InteractionResponsePayload{RequestID: requestID, Kind: protocol.InteractionCommandApproval, OptionID: optionID, Answers: map[string][]string{}})
	resolved, responded := false, false
	for !resolved || !responded {
		event := s.read(t)
		switch event["type"] {
		case string(protocol.EventInteractionResolved):
			payload := event["payload"].(map[string]any)
			if payload["request_id"] == requestID {
				require.Equal(t, optionID, payload["option_id"])
				resolved = true
			}
		case string(protocol.EventCommandResult):
			payload := event["payload"].(map[string]any)
			if payload["command_id"] == responseID {
				require.Equal(t, string(protocol.CommandSucceeded), payload["status"], "%#v", payload)
				responded = true
			}
		}
	}
	evidence := h.waitEvidence(t, "permission.semantic")
	require.Contains(t, evidence, `"permission_outcome":"reject"`)

	secondID := s.command(t, protocol.CommandInteractionRespond, protocol.InteractionResponsePayload{RequestID: requestID, Kind: protocol.InteractionCommandApproval, OptionID: optionID, Answers: map[string][]string{}})
	second := s.waitResult(t, secondID)
	require.Equal(t, string(protocol.CommandRejected), second["status"])
}

func cursorSubmitPayload(h *cursorHarness, resourceID, turnID, messageID, text string, images []protocol.ImageReference) protocol.SubmitPayload {
	created := time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC)
	settings := protocol.ExecutionSettings{Model: "cursor-large", Effort: "default", Speed: protocol.SpeedStandard}
	return protocol.SubmitPayload{
		TurnID: turnID, MessageID: messageID,
		Content: protocol.MessageContent{Parts: []protocol.MessagePart{{Type: protocol.MessagePartText, Text: text}}}, Images: images,
		Context:  &protocol.PageContext{Revision: protocol.ContextInitial, Source: cursorSource, CreatorContext: cursorCreator, Title: "Fixture", URL: h.origin + "/fixture", Resource: protocol.Resource{Kind: protocol.ResourceMarkdown, ID: resourceID, CreatedAt: created, UpdatedAt: created}, Digest: agent.CalculateContextDigest([]byte(cursorSource), []byte(cursorCreator))},
		Settings: &settings,
	}
}

func TestCursorFirstPromptCanReplaceAnUnlistedPromptFreeNativeSessionForModelSelection(t *testing.T) {
	h := newCursorHarness(t, "ready_hide_unprompted")
	s := h.connect(t)
	defer s.ws.Close()

	payload := cursorSubmitPayload(h, cursorID('C'), cursorID('M'), cursorID('N'), "first prompt after model choice", nil)
	payload.Settings = &protocol.ExecutionSettings{Model: "cursor-small", Effort: "default", Speed: protocol.SpeedStandard}
	commandID := s.command(t, protocol.CommandSubmit, payload)
	result := s.waitResult(t, commandID)
	require.Equal(t, string(protocol.CommandSucceeded), result["status"], "%#v", result)

	evidence := h.waitEvidence(t, `"method":"session/prompt.semantic"`)
	require.GreaterOrEqual(t, strings.Count(evidence, `"method":"session/new.semantic"`), 2, evidence)
	require.Contains(t, evidence, `"launch_model":"cursor-small"`)
	require.Equal(t, 1, strings.Count(evidence, `"method":"session/prompt.semantic"`), evidence)
	require.NotContains(t, evidence, "session/set_config_option.semantic")
}

func TestCursorInterruptCancelsPromptAndPendingPermission(t *testing.T) {
	h := newCursorHarness(t, "permission_hold_cancel")
	s := h.connect(t)
	defer s.ws.Close()
	turnID := cursorID('M')
	submitID := s.command(t, protocol.CommandSubmit, cursorSubmitPayload(h, cursorID('C'), turnID, cursorID('N'), "cancel question", nil))
	interactionSeen, accepted, workID := false, false, ""
	for !interactionSeen || !accepted || workID == "" {
		event := s.read(t)
		switch event["type"] {
		case string(protocol.EventInteractionRequest):
			interactionSeen = true
		case string(protocol.EventLifecycle):
			payload := event["payload"].(map[string]any)
			if active, ok := payload["active_work"].(map[string]any); ok {
				workID, _ = active["work_id"].(string)
			}
		case string(protocol.EventCommandResult):
			payload := event["payload"].(map[string]any)
			if payload["command_id"] == submitID {
				require.Equal(t, string(protocol.CommandSucceeded), payload["status"])
				accepted = true
			}
		}
	}
	interruptID := s.command(t, protocol.CommandInterrupt, protocol.WorkReferencePayload{WorkID: workID})
	interrupted, resolved, stopped := false, false, false
	for !interrupted || !resolved || !stopped {
		event := s.read(t)
		switch event["type"] {
		case string(protocol.EventInteractionResolved):
			payload := event["payload"].(map[string]any)
			require.Empty(t, payload["option_id"])
			resolved = true
		case string(protocol.EventInterruption):
			interrupted = true
		case string(protocol.EventCommandResult):
			payload := event["payload"].(map[string]any)
			if payload["command_id"] == interruptID {
				require.Equal(t, string(protocol.CommandSucceeded), payload["status"])
				stopped = true
			}
		}
	}
	evidence := h.waitEvidence(t, "session/cancel.semantic")
	require.Contains(t, evidence, `"cancel_outcome":"received"`)
	evidence = h.waitEvidence(t, "permission.semantic")
	require.Contains(t, evidence, `"permission_outcome":"cancelled"`)
	require.Equal(t, 1, strings.Count(evidence, "session/prompt.semantic"), "cancelled prompt was automatically replayed")
}

func TestCursorArchiveRestoreLoadReplayHistoryAndUnsupportedDelete(t *testing.T) {
	h := newCursorHarness(t, "ready")
	first := h.connect(t)
	turnID := cursorID('P')
	submitID := first.command(t, protocol.CommandSubmit, cursorSubmitPayload(h, cursorID('C'), turnID, cursorID('Q'), "archive reader", nil))
	completed, accepted := false, false
	for !completed || !accepted {
		event := first.read(t)
		switch event["type"] {
		case string(protocol.EventCompletion):
			completed = true
		case string(protocol.EventCommandResult):
			payload := event["payload"].(map[string]any)
			if payload["command_id"] == submitID {
				require.Equal(t, string(protocol.CommandSucceeded), payload["status"])
				accepted = true
			}
		}
	}
	newID := first.command(t, protocol.CommandNew, protocol.NewPayload{Settings: &protocol.ExecutionSettings{Model: "cursor-large", Effort: "default", Speed: protocol.SpeedStandard}})
	archived, handedOff := false, false
	for !archived || !handedOff {
		event := first.read(t)
		switch event["type"] {
		case string(protocol.EventArchive):
			archived = event["payload"].(map[string]any)["action"] == string(protocol.ArchiveCreated)
		case string(protocol.EventCommandResult):
			payload := event["payload"].(map[string]any)
			if payload["command_id"] == newID {
				require.Equal(t, string(protocol.CommandSucceeded), payload["status"])
				handedOff = true
			}
		}
	}
	require.Eventually(t, func() bool {
		b, _ := os.ReadFile(h.evidencePath)
		return strings.Count(string(b), "session/new.semantic") >= 2
	}, processTimeout, pollInterval)
	_ = first.ws.Close()

	current := h.connect(t)
	defer current.ws.Close()
	listID := current.command(t, protocol.CommandArchiveList, protocol.PageRequestPayload{Limit: 10})
	var archiveID string
	listed, listResult := false, false
	for !listed || !listResult {
		event := current.read(t)
		switch event["type"] {
		case string(protocol.EventHistory):
			items := event["payload"].(map[string]any)["items"].([]any)
			require.NotEmpty(t, items)
			archiveID = items[0].(map[string]any)["archive_id"].(string)
			listed = true
		case string(protocol.EventCommandResult):
			payload := event["payload"].(map[string]any)
			if payload["command_id"] == listID {
				require.Equal(t, string(protocol.CommandSucceeded), payload["status"])
				listResult = true
			}
		}
	}
	require.NotEmpty(t, archiveID)
	workspaceRoot := filepath.Join(h.home, ".agent-whiteboard", "state", "workspaces")
	beforeWorkspaces, err := os.ReadDir(workspaceRoot)
	require.NoError(t, err)
	beforeEvidence, err := os.ReadFile(h.evidencePath)
	require.NoError(t, err)
	deleteID := current.command(t, protocol.CommandArchiveDelete, protocol.ArchiveReferencePayload{ArchiveID: archiveID})
	deleted := current.waitResult(t, deleteID)
	require.Equal(t, string(protocol.CommandRejected), deleted["status"])
	require.Equal(t, "archive_delete_unsupported", deleted["error"].(map[string]any)["code"])
	afterEvidence, err := os.ReadFile(h.evidencePath)
	require.NoError(t, err)
	require.Equal(t, strings.Count(string(beforeEvidence), "session/delete"), strings.Count(string(afterEvidence), "session/delete"))
	afterWorkspaces, err := os.ReadDir(workspaceRoot)
	require.NoError(t, err)
	require.Len(t, afterWorkspaces, len(beforeWorkspaces), "unsupported delete changed workspaces")

	relistID := current.command(t, protocol.CommandArchiveList, protocol.PageRequestPayload{Limit: 10})
	retained, relisted := false, false
	for !retained || !relisted {
		event := current.read(t)
		switch event["type"] {
		case string(protocol.EventHistory):
			for _, raw := range event["payload"].(map[string]any)["items"].([]any) {
				if raw.(map[string]any)["archive_id"] == archiveID {
					retained = true
				}
			}
		case string(protocol.EventCommandResult):
			payload := event["payload"].(map[string]any)
			if payload["command_id"] == relistID {
				require.Equal(t, string(protocol.CommandSucceeded), payload["status"])
				relisted = true
			}
		}
	}

	loadsBefore := strings.Count(string(afterEvidence), "session/load.semantic")
	restoreID := current.command(t, protocol.CommandArchiveRestore, protocol.ArchiveReferencePayload{ArchiveID: archiveID})
	restoredEvent, restoreResult := false, false
	for !restoredEvent || !restoreResult {
		event := current.read(t)
		switch event["type"] {
		case string(protocol.EventArchive):
			restoredEvent = event["payload"].(map[string]any)["action"] == string(protocol.ArchiveRestored)
		case string(protocol.EventCommandResult):
			payload := event["payload"].(map[string]any)
			if payload["command_id"] == restoreID {
				require.Equal(t, string(protocol.CommandSucceeded), payload["status"])
				restoreResult = true
			}
		}
	}
	require.Eventually(t, func() bool {
		b, _ := os.ReadFile(h.evidencePath)
		return strings.Count(string(b), "session/load.semantic") > loadsBefore && strings.Contains(string(b), `"content_block_count":2`)
	}, processTimeout, pollInterval)
	_ = current.ws.Close()

	restored := h.connect(t)
	defer restored.ws.Close()
	historyID := restored.command(t, protocol.CommandHistoryPage, protocol.PageRequestPayload{Limit: 10})
	timelineSeen, historyResult := false, false
	for !timelineSeen || !historyResult {
		event := restored.read(t)
		switch event["type"] {
		case string(protocol.EventTimeline):
			encoded, err := json.Marshal(event["payload"])
			require.NoError(t, err)
			require.Contains(t, string(encoded), "archive reader")
			require.Contains(t, string(encoded), "scripted answer")
			timelineSeen = true
		case string(protocol.EventCommandResult):
			payload := event["payload"].(map[string]any)
			if payload["command_id"] == historyID {
				require.Equal(t, string(protocol.CommandSucceeded), payload["status"])
				historyResult = true
			}
		}
	}
	evidence := h.waitEvidence(t, "session/load.semantic")
	require.Contains(t, evidence, "session/list")
	require.GreaterOrEqual(t, strings.Count(evidence, "session/load.semantic"), 1, "restore must load through ACP")
}

func TestCursorClosedStreamCompletesWithoutRedErrorRestartOrReplay(t *testing.T) {
	h := newCursorHarness(t, "closed_stream")
	s := h.connect(t)
	defer s.ws.Close()
	initialEvidence := h.waitEvidence(t, "session/new.semantic")
	processStarts := strings.Count(initialEvidence, "process_start")
	loads := strings.Count(initialEvidence, "session/load.semantic")
	created := time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC)
	digest := agent.CalculateContextDigest([]byte(cursorSource), []byte(cursorCreator))
	turnID, messageID := cursorID('K'), cursorID('L')
	commandID := s.command(t, protocol.CommandSubmit, protocol.SubmitPayload{TurnID: turnID, MessageID: messageID, Content: protocol.MessageContent{Parts: []protocol.MessagePart{{Type: protocol.MessagePartText, Text: "runtime failure"}}}, Context: &protocol.PageContext{Revision: protocol.ContextInitial, Source: cursorSource, CreatorContext: cursorCreator, Title: "Failure", URL: h.origin + "/failure", Resource: protocol.Resource{Kind: protocol.ResourceMarkdown, ID: cursorID('C'), CreatedAt: created, UpdatedAt: created}, Digest: digest}, Settings: &protocol.ExecutionSettings{Model: "cursor-large", Effort: "default", Speed: protocol.SpeedStandard}})

	waitForCompletion := func(commandID, turnID string) {
		commandSucceeded, completed := false, false
		for !commandSucceeded || !completed {
			event := s.read(t)
			encoded, err := json.Marshal(event)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "RetriableError")
			require.NotContains(t, string(encoded), "WritableIterable")
			switch event["type"] {
			case string(protocol.EventCommandResult):
				payload := event["payload"].(map[string]any)
				if payload["command_id"] == commandID {
					require.Equal(t, string(protocol.CommandSucceeded), payload["status"])
					commandSucceeded = true
				}
			case string(protocol.EventCompletion):
				if event["payload"].(map[string]any)["turn_id"] == turnID {
					completed = true
				}
			case string(protocol.EventError):
				t.Fatalf("closed-stream recovery rendered an error: %#v", event["payload"])
			case string(protocol.EventLifecycle):
				state, _ := event["payload"].(map[string]any)["state"].(string)
				require.NotEqual(t, string(protocol.LifecycleUnavailable), state, "closed-stream recovery restarted the provider")
			}
		}
	}
	waitForCompletion(commandID, turnID)

	secondTurn, secondMessage := cursorID('M'), cursorID('N')
	secondCommand := s.command(t, protocol.CommandSubmit, protocol.SubmitPayload{TurnID: secondTurn, MessageID: secondMessage, Content: protocol.MessageContent{Parts: []protocol.MessagePart{{Type: protocol.MessagePartText, Text: "second prompt"}}}, Settings: &protocol.ExecutionSettings{Model: "cursor-large", Effort: "default", Speed: protocol.SpeedStandard}})
	waitForCompletion(secondCommand, secondTurn)

	require.Eventually(t, func() bool {
		b, _ := os.ReadFile(h.evidencePath)
		return strings.Count(string(b), "session/prompt.semantic") == 2
	}, 3*time.Second, 10*time.Millisecond)
	evidenceBytes, err := os.ReadFile(h.evidencePath)
	require.NoError(t, err)
	evidence := string(evidenceBytes)
	require.Equal(t, processStarts, strings.Count(evidence, "process_start"), "closed-stream recovery launched another process")
	require.Equal(t, loads, strings.Count(evidence, "session/load.semantic"), "closed-stream recovery loaded instead of retaining the native session")
	require.Equal(t, 2, strings.Count(evidence, "session/prompt.semantic"))
	require.Equal(t, 1, strings.Count(evidence, "session/prompt.runtime_failure"))
	require.Equal(t, 1, strings.Count(evidence, "session/cancel.semantic"))
}

func TestCursorImageSemanticsManagedShutdownAndCrashIsolation(t *testing.T) {
	t.Run("image semantics and managed shutdown", func(t *testing.T) {
		h := newCursorHarness(t, "hold_cancel")
		s := h.connect(t)
		defer s.ws.Close()
		image, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
		require.NoError(t, err)
		request, err := http.NewRequest(http.MethodPost, h.baseURL+protocol.ImagesPath, bytes.NewReader(image))
		require.NoError(t, err)
		request.Header.Set("Origin", h.origin)
		request.Header.Set("Content-Type", "image/png")
		request.Header.Set(protocol.APIVersionHeader, protocol.APIVersion)
		request.Header.Set(protocol.ClientIDHeader, s.clientID)
		request.Header.Set(protocol.ConversationIDHeader, s.conversationID)
		request.Header.Set(protocol.ProviderHeader, "cursor")
		request.Header.Set(protocol.ImagePurposeHeader, string(attachment.PurposeAttachment))
		resp, err := http.DefaultClient.Do(request)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		var staged struct {
			ImageID string `json:"image_id"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&staged))
		created := time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC)
		source, creator := cursorSource, cursorCreator
		digest := agent.CalculateContextDigest([]byte(source), []byte(creator))
		turn := cursorID('F')
		s.command(t, protocol.CommandSubmit, protocol.SubmitPayload{TurnID: turn, MessageID: cursorID('G'), Content: protocol.MessageContent{Parts: []protocol.MessagePart{{Type: protocol.MessagePartText, Text: "inspect"}}}, Images: []protocol.ImageReference{{ImageID: staged.ImageID, Name: "fixture.png"}}, Context: &protocol.PageContext{Revision: protocol.ContextInitial, Source: source, CreatorContext: creator, Title: "Image", URL: h.origin + "/image", Resource: protocol.Resource{Kind: protocol.ResourceMarkdown, ID: cursorID('C'), CreatedAt: created, UpdatedAt: created}, Digest: digest}, Settings: &protocol.ExecutionSettings{Model: "cursor-large", Effort: "default", Speed: protocol.SpeedStandard}})
		h.waitEvidence(t, "session/prompt.semantic")
		require.NoError(t, h.service.Close())
		evidence := h.waitEvidence(t, "session/prompt.semantic")
		require.Contains(t, evidence, `"image_media_type":"image/png"`)
		require.Contains(t, evidence, fmt.Sprintf(`"image_decoded_byte_count":%d`, len(image)))
		require.NotContains(t, evidence, staged.ImageID)
		require.NotContains(t, evidence, "fixture.png")
	})
	t.Run("crash affects only owning conversation", func(t *testing.T) {
		h := newCursorHarness(t, "crash_prompt")
		first := h.connect(t)
		defer first.ws.Close()
		second := h.connectResource(t, cursorID('J'))
		defer second.ws.Close()
		require.NotEqual(t, first.conversationID, second.conversationID)
		created := time.Date(2026, 8, 29, 1, 2, 3, 0, time.UTC)
		source, creator := cursorSource, cursorCreator
		digest := agent.CalculateContextDigest([]byte(source), []byte(creator))
		first.command(t, protocol.CommandSubmit, protocol.SubmitPayload{TurnID: cursorID('H'), MessageID: cursorID('I'), Content: protocol.MessageContent{Parts: []protocol.MessagePart{{Type: protocol.MessagePartText, Text: "crash"}}}, Context: &protocol.PageContext{Revision: protocol.ContextInitial, Source: source, CreatorContext: creator, Title: "Crash", URL: h.origin + "/crash", Resource: protocol.Resource{Kind: protocol.ResourceMarkdown, ID: cursorID('C'), CreatedAt: created, UpdatedAt: created}, Digest: digest}, Settings: &protocol.ExecutionSettings{Model: "cursor-large", Effort: "default", Speed: protocol.SpeedStandard}})
		h.waitEvidence(t, "session/prompt.semantic")
		require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(h.control, "crash.ready")); return err == nil }, processTimeout, pollInterval)
		require.NoError(t, os.WriteFile(filepath.Join(h.control, "crash.release"), []byte("release"), 0o600))
		for {
			event := first.read(t)
			if event["type"] == string(protocol.EventError) {
				code, _ := event["payload"].(map[string]any)["error"].(map[string]any)["code"].(string)
				require.Contains(t, []string{string(protocol.ErrorAcceptanceOutcomeUnknown), "provider_crashed"}, code)
				break
			}
		}
		newID := second.command(t, protocol.CommandNew, protocol.NewPayload{})
		require.Equal(t, string(protocol.CommandSucceeded), second.waitResult(t, newID)["status"])
		evidence := h.waitEvidence(t, "session/new.semantic")
		require.GreaterOrEqual(t, strings.Count(evidence, "session/new.semantic"), 2)
	})
}

func (h *cursorHarness) waitEvidence(t *testing.T, needle string) string {
	t.Helper()
	deadline := time.Now().Add(processTimeout)
	for {
		b, _ := os.ReadFile(h.evidencePath)
		if strings.Contains(string(b), needle) {
			return string(b)
		}
		if time.Now().After(deadline) {
			require.FailNow(t, "timed out waiting for Cursor fixture evidence", "needle %s; evidence %s", needle, b)
		}
		time.Sleep(pollInterval)
	}
}
