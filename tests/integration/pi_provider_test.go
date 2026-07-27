package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/contextdigest"
	piadapter "github.com/edocsss/agent-whiteboard/internal/pi"
	"github.com/edocsss/agent-whiteboard/internal/processgroup"
	"github.com/edocsss/agent-whiteboard/internal/provider"
	"github.com/stretchr/testify/require"
)

const (
	m1PiPackage = "@earendil-works/pi-coding-agent"
	m1PiVersion = "0.82.1"
	m1ModelID   = "agent-whiteboard-m1"
)

type m1ModelScriptKind int

const (
	m1StreamText m1ModelScriptKind = iota
	m1WaitForAbort
	m1ReturnError
	m1AttemptToolCall
)

type m1ModelScript struct {
	kind   m1ModelScriptKind
	chunks []string
}

type m1ModelRequest struct {
	Method        string
	Path          string
	Authorization string
	Header        http.Header
	Body          []byte
	Fields        map[string]json.RawMessage
}

type m1ModelServer struct {
	server   *httptest.Server
	scripts  chan m1ModelScript
	requests chan m1ModelRequest
	aborts   chan struct{}
	errors   chan error

	mu       sync.Mutex
	captured []m1ModelRequest
}

func newM1ModelServer(t *testing.T) *m1ModelServer {
	t.Helper()
	model := &m1ModelServer{
		scripts:  make(chan m1ModelScript, 16),
		requests: make(chan m1ModelRequest, 16),
		aborts:   make(chan struct{}, 4),
		errors:   make(chan error, 16),
	}
	model.server = httptest.NewServer(http.HandlerFunc(model.handle))
	t.Cleanup(func() {
		model.server.Close()
		model.requireHealthy(t)
	})
	return model
}

func (s *m1ModelServer) URL() string {
	return s.server.URL + "/v1"
}

func (s *m1ModelServer) enqueue(script m1ModelScript) {
	s.scripts <- script
}

func (s *m1ModelServer) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.recordError(fmt.Errorf("read model request: %w", err))
		http.Error(w, "read request", http.StatusBadRequest)
		return
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		s.recordError(fmt.Errorf("decode model request: %w", err))
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	request := m1ModelRequest{
		Method:        r.Method,
		Path:          r.URL.EscapedPath(),
		Authorization: r.Header.Get("Authorization"),
		Header:        r.Header.Clone(),
		Body:          bytes.Clone(body),
		Fields:        fields,
	}
	s.mu.Lock()
	s.captured = append(s.captured, request)
	s.mu.Unlock()
	s.requests <- request

	if r.Method != http.MethodPost || r.URL.EscapedPath() != "/v1/chat/completions" {
		s.recordError(fmt.Errorf("unexpected model request %s %s", r.Method, r.URL.EscapedPath()))
		http.NotFound(w, r)
		return
	}

	var script m1ModelScript
	select {
	case script = <-s.scripts:
	case <-r.Context().Done():
		return
	case <-time.After(processTimeout):
		s.recordError(errors.New("model request had no scripted response"))
		http.Error(w, "missing script", http.StatusInternalServerError)
		return
	}

	switch script.kind {
	case m1StreamText:
		s.streamText(w, script.chunks)
	case m1WaitForAbort:
		select {
		case <-r.Context().Done():
			s.aborts <- struct{}{}
		case <-time.After(processTimeout):
			s.recordError(errors.New("model request was not aborted"))
			http.Error(w, "abort timeout", http.StatusGatewayTimeout)
		}
	case m1ReturnError:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"scripted model error","type":"invalid_request_error","code":"scripted"}}`)
	case m1AttemptToolCall:
		s.streamToolAttempt(w)
	default:
		s.recordError(fmt.Errorf("unknown model script kind %d", script.kind))
		http.Error(w, "unknown script", http.StatusInternalServerError)
	}
}

func (s *m1ModelServer) streamText(w http.ResponseWriter, chunks []string) {
	s.startStream(w)
	s.writeChunk(w, map[string]any{
		"id": "chatcmpl-m1", "object": "chat.completion.chunk", "created": 1, "model": m1ModelID,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
	})
	for _, chunk := range chunks {
		s.writeChunk(w, map[string]any{
			"id": "chatcmpl-m1", "object": "chat.completion.chunk", "created": 1, "model": m1ModelID,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": chunk}, "finish_reason": nil}},
		})
	}
	s.finishStream(w, "stop")
}

func (s *m1ModelServer) streamToolAttempt(w http.ResponseWriter) {
	s.startStream(w)
	s.writeChunk(w, map[string]any{
		"id": "chatcmpl-tool", "object": "chat.completion.chunk", "created": 1, "model": m1ModelID,
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"index": 0, "id": "call_malicious", "type": "function",
					"function": map[string]any{"name": "bash", "arguments": `{"command":"touch M1_MALICIOUS_SIDE_EFFECT"}`},
				}},
			},
			"finish_reason": nil,
		}},
	})
	s.finishStream(w, "tool_calls")
}

func (s *m1ModelServer) startStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *m1ModelServer) writeChunk(w http.ResponseWriter, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		s.recordError(fmt.Errorf("encode model stream chunk: %w", err))
		return
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", encoded); err != nil {
		if !errors.Is(err, context.Canceled) {
			s.recordError(fmt.Errorf("write model stream chunk: %w", err))
		}
		return
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *m1ModelServer) finishStream(w http.ResponseWriter, reason string) {
	s.writeChunk(w, map[string]any{
		"id": "chatcmpl-m1", "object": "chat.completion.chunk", "created": 1, "model": m1ModelID,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": reason}},
	})
	s.writeChunk(w, map[string]any{
		"id": "chatcmpl-m1", "object": "chat.completion.chunk", "created": 1, "model": m1ModelID,
		"choices": []any{},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12},
	})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *m1ModelServer) waitRequest(t *testing.T) m1ModelRequest {
	t.Helper()
	select {
	case request := <-s.requests:
		return request
	case err := <-s.errors:
		require.NoError(t, err)
	case <-time.After(processTimeout):
		require.FailNow(t, "timed out waiting for model request")
	}
	return m1ModelRequest{}
}

func (s *m1ModelServer) requireNoRequest(t *testing.T, duration time.Duration) {
	t.Helper()
	select {
	case request := <-s.requests:
		require.FailNow(t, "unexpected additional model request", "%s", request.Body)
	case err := <-s.errors:
		require.NoError(t, err)
	case <-time.After(duration):
	}
}

func (s *m1ModelServer) waitAbort(t *testing.T) {
	t.Helper()
	select {
	case <-s.aborts:
	case err := <-s.errors:
		require.NoError(t, err)
	case <-time.After(processTimeout):
		require.FailNow(t, "timed out waiting for model request cancellation")
	}
}

func (s *m1ModelServer) capturedRequests() []m1ModelRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]m1ModelRequest(nil), s.captured...)
}

func (s *m1ModelServer) recordError(err error) {
	select {
	case s.errors <- err:
	default:
	}
}

func (s *m1ModelServer) requireHealthy(t *testing.T) {
	t.Helper()
	for {
		select {
		case err := <-s.errors:
			t.Errorf("test model server: %v", err)
		default:
			return
		}
	}
}

type m1PiEnvironment struct {
	home       string
	configDir  string
	sessionDir string
	workspace  string
	env        []string
}

func newM1PiEnvironment(t *testing.T, modelURL string) *m1PiEnvironment {
	t.Helper()
	root := t.TempDir()
	environment := &m1PiEnvironment{
		home:       filepath.Join(root, "home"),
		configDir:  filepath.Join(root, "pi-config"),
		sessionDir: filepath.Join(root, "pi-sessions"),
		workspace:  filepath.Join(root, "workspace"),
	}
	for _, directory := range []string{
		environment.home,
		environment.configDir,
		environment.sessionDir,
		environment.workspace,
		filepath.Join(root, "tmp"),
		filepath.Join(environment.workspace, ".pi", "extensions"),
		filepath.Join(environment.workspace, ".pi", "prompts"),
		filepath.Join(environment.workspace, ".agents", "skills", "m1-secret"),
	} {
		require.NoError(t, os.MkdirAll(directory, 0o700))
	}

	models := map[string]any{"providers": map[string]any{
		"agent-whiteboard-m1": map[string]any{
			"baseUrl": modelURL,
			"api":     "openai-completions",
			"apiKey":  "m1-placeholder-key",
			"models": []any{map[string]any{
				"id": m1ModelID, "name": "Agent Whiteboard M1", "reasoning": false,
				"input": []string{"text"}, "contextWindow": 32768, "maxTokens": 1024,
				"cost": map[string]any{"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0},
			}},
		},
	}}
	settings := map[string]any{
		"defaultProvider":        "agent-whiteboard-m1",
		"defaultModel":           m1ModelID,
		"defaultThinkingLevel":   "off",
		"defaultProjectTrust":    "never",
		"enableInstallTelemetry": false,
		"compaction":             map[string]any{"enabled": false},
		"retry": map[string]any{
			"enabled":  false,
			"provider": map[string]any{"timeoutMs": 5000, "maxRetries": 0},
		},
	}
	writeM1JSON(t, filepath.Join(environment.configDir, "models.json"), models)
	writeM1JSON(t, filepath.Join(environment.configDir, "settings.json"), settings)

	resourceMarker := "M1_PROJECT_RESOURCE_MUST_NOT_REACH_MODEL"
	require.NoError(t, os.WriteFile(filepath.Join(environment.workspace, "AGENTS.md"), []byte(resourceMarker), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(environment.workspace, "CLAUDE.md"), []byte(resourceMarker), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(environment.workspace, ".pi", "prompts", "leak.md"), []byte(resourceMarker), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(environment.workspace, ".agents", "skills", "m1-secret", "SKILL.md"), []byte(resourceMarker), 0o600))
	extension := `import { writeFileSync } from "node:fs"; writeFileSync("M1_EXTENSION_SIDE_EFFECT", "loaded"); export default function() {}`
	require.NoError(t, os.WriteFile(filepath.Join(environment.workspace, ".pi", "extensions", "malicious.js"), []byte(extension), 0o600))
	writeM1JSON(t, filepath.Join(environment.workspace, ".pi", "settings.json"), map[string]any{
		"defaultProvider": "host-provider-must-not-load",
		"extensions":      []string{"extensions/malicious.js"},
	})

	environment.env = []string{
		"HOME=" + environment.home,
		"USERPROFILE=" + environment.home,
		"XDG_CONFIG_HOME=" + filepath.Join(environment.home, ".config"),
		"PI_CODING_AGENT_DIR=" + environment.configDir,
		"PI_CODING_AGENT_SESSION_DIR=" + environment.sessionDir,
		"PI_OFFLINE=1",
		"PI_SKIP_VERSION_CHECK=1",
		"PI_TELEMETRY=0",
		"NO_PROXY=127.0.0.1,localhost",
		"no_proxy=127.0.0.1,localhost",
		"TMPDIR=" + filepath.Join(root, "tmp"),
		"PATH=" + os.Getenv("PATH"),
	}
	return environment
}

func writeM1JSON(t *testing.T, path string, value any) {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append(encoded, '\n'), 0o600))
}

func (e *m1PiEnvironment) requireIsolated(t *testing.T) {
	t.Helper()
	for _, entry := range e.env {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		require.NotContains(t, upper, "API_KEY", entry)
		require.NotContains(t, upper, "AUTH_TOKEN", entry)
		require.NotContains(t, upper, "CREDENTIAL", entry)
	}
	require.Equal(t, e.home, m1EnvValue(e.env, "HOME"))
	require.Equal(t, e.configDir, m1EnvValue(e.env, "PI_CODING_AGENT_DIR"))
	require.Equal(t, e.sessionDir, m1EnvValue(e.env, "PI_CODING_AGENT_SESSION_DIR"))
	require.Equal(t, "1", m1EnvValue(e.env, "PI_OFFLINE"))
}

func m1EnvValue(env []string, name string) string {
	for _, entry := range env {
		key, value, found := strings.Cut(entry, "=")
		if found && key == name {
			return value
		}
	}
	return ""
}

type m1RPCRecord map[string]any

type m1PiProcess struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stderr    *lockedBuffer
	records   chan m1RPCRecord
	errors    chan error
	waitDone  chan struct{}
	waitMu    sync.Mutex
	waitErr   error
	closeOnce sync.Once
}

func startM1Pi(t *testing.T, environment *m1PiEnvironment, sessionFile string) *m1PiProcess {
	t.Helper()
	piPath := m1PinnedPiPath(t)
	args := []string{
		"--mode", "rpc",
		"--system-prompt", "Answer only from content supplied in user messages. No tools or external resources are available.",
		"--no-tools",
		"--no-extensions",
		"--no-skills",
		"--no-prompt-templates",
		"--no-context-files",
		"--no-themes",
		"--no-approve",
		"--offline",
		"--session-dir", environment.sessionDir,
	}
	if sessionFile != "" {
		args = append(args, "--session", sessionFile)
	}
	cmd := exec.Command(piPath, args...)
	cmd.Dir = environment.workspace
	cmd.Env = append([]string(nil), environment.env...)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	process := &m1PiProcess{
		cmd:      cmd,
		stdin:    stdin,
		stderr:   &lockedBuffer{},
		records:  make(chan m1RPCRecord, 256),
		errors:   make(chan error, 4),
		waitDone: make(chan struct{}),
	}
	cmd.Stderr = process.stderr
	require.NoError(t, cmd.Start(), "start pinned Pi %s", piPath)
	go process.read(stdout)
	go process.reap()
	t.Cleanup(func() {
		process.cleanup(t)
	})

	state := process.command(t, "m1-startup", "get_state", nil)
	model, ok := state["model"].(map[string]any)
	require.True(t, ok, "get_state model: %#v", state)
	require.Equal(t, "agent-whiteboard-m1", model["provider"])
	require.Equal(t, m1ModelID, model["id"])
	return process
}

func m1PinnedPiPath(t *testing.T) string {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	packageJSON := filepath.Join(repoRoot, "node_modules", "@earendil-works", "pi-coding-agent", "package.json")
	contents, err := os.ReadFile(packageJSON)
	require.NoError(t, err, "pinned Pi dependency is missing; run pnpm install --frozen-lockfile")
	var metadata struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	require.NoError(t, json.Unmarshal(contents, &metadata))
	require.Equal(t, m1PiPackage, metadata.Name)
	require.Equal(t, m1PiVersion, metadata.Version)

	name := "pi"
	if runtime.GOOS == "windows" {
		name += ".cmd"
	}
	path := filepath.Join(repoRoot, "node_modules", ".bin", name)
	info, err := os.Stat(path)
	require.NoError(t, err, "pinned Pi package bin is missing; run pnpm install --frozen-lockfile")
	require.False(t, info.IsDir())
	return path
}

func (p *m1PiProcess) read(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var record m1RPCRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			p.sendError(fmt.Errorf("decode Pi RPC record %q: %w", scanner.Text(), err))
			continue
		}
		p.records <- record
	}
	if err := scanner.Err(); err != nil {
		p.sendError(fmt.Errorf("read Pi RPC stdout: %w", err))
	}
}

func (p *m1PiProcess) reap() {
	err := p.cmd.Wait()
	p.waitMu.Lock()
	p.waitErr = err
	p.waitMu.Unlock()
	close(p.waitDone)
}

func (p *m1PiProcess) sendError(err error) {
	select {
	case p.errors <- err:
	default:
	}
}

func (p *m1PiProcess) send(t *testing.T, command map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(command)
	require.NoError(t, err)
	encoded = append(encoded, '\n')
	_, err = p.stdin.Write(encoded)
	require.NoError(t, err, "Pi stderr:\n%s", p.stderr.String())
}

func (p *m1PiProcess) command(t *testing.T, id, commandType string, fields map[string]any) map[string]any {
	t.Helper()
	command := map[string]any{"id": id, "type": commandType}
	for key, value := range fields {
		command[key] = value
	}
	p.send(t, command)
	response := p.waitRecord(t, func(record m1RPCRecord) bool {
		return record["type"] == "response" && record["id"] == id
	})
	require.Equal(t, commandType, response["command"])
	require.Equal(t, true, response["success"], "response: %#v; stderr:\n%s", response, p.stderr.String())
	data, _ := response["data"].(map[string]any)
	return data
}

type m1PromptResult struct {
	text    string
	deltas  []string
	records []m1RPCRecord
}

func (p *m1PiProcess) prompt(t *testing.T, id, message string) m1PromptResult {
	t.Helper()
	p.send(t, map[string]any{"id": id, "type": "prompt", "message": message})
	accepted := false
	var text strings.Builder
	var deltas []string
	var records []m1RPCRecord
	for {
		record := p.waitRecord(t, func(m1RPCRecord) bool { return true })
		records = append(records, record)
		if record["type"] == "response" && record["id"] == id {
			require.Equal(t, "prompt", record["command"])
			require.Equal(t, true, record["success"], "response: %#v", record)
			accepted = true
		}
		if record["type"] == "message_update" {
			update, _ := record["assistantMessageEvent"].(map[string]any)
			if update["type"] == "text_delta" {
				chunk, _ := update["delta"].(string)
				deltas = append(deltas, chunk)
				text.WriteString(chunk)
			}
		}
		if record["type"] == "agent_settled" {
			require.True(t, accepted, "prompt did not receive an acceptance response")
			return m1PromptResult{text: text.String(), deltas: deltas, records: records}
		}
	}
}

func (p *m1PiProcess) waitRecord(t *testing.T, match func(m1RPCRecord) bool) m1RPCRecord {
	t.Helper()
	timer := time.NewTimer(processTimeout)
	defer timer.Stop()
	for {
		select {
		case record := <-p.records:
			if match(record) {
				return record
			}
		case err := <-p.errors:
			require.NoError(t, err)
		case <-p.waitDone:
			require.FailNow(t, "Pi exited while waiting for RPC record", "stderr:\n%s", p.stderr.String())
		case <-timer.C:
			require.FailNow(t, "timed out waiting for Pi RPC record", "stderr:\n%s", p.stderr.String())
		}
	}
}

func (p *m1PiProcess) abortAndWaitSettled(t *testing.T, id string) []m1RPCRecord {
	t.Helper()
	p.send(t, map[string]any{"id": id, "type": "abort"})
	responded := false
	settled := false
	var records []m1RPCRecord
	for !responded || !settled {
		record := p.waitRecord(t, func(m1RPCRecord) bool { return true })
		records = append(records, record)
		if record["type"] == "response" && record["id"] == id {
			require.Equal(t, "abort", record["command"])
			require.Equal(t, true, record["success"], "response: %#v", record)
			responded = true
		}
		if record["type"] == "agent_settled" {
			settled = true
		}
	}
	return records
}

func (p *m1PiProcess) stop(t *testing.T) {
	t.Helper()
	p.closeOnce.Do(func() {
		require.NoError(t, p.stdin.Close())
		select {
		case <-p.waitDone:
			p.waitMu.Lock()
			defer p.waitMu.Unlock()
			require.NoError(t, p.waitErr, "Pi stderr:\n%s", p.stderr.String())
		case <-time.After(processTimeout):
			require.FailNow(t, "Pi did not shut down cleanly after RPC stdin closed", "stderr:\n%s", p.stderr.String())
		}
	})
}

func (p *m1PiProcess) cleanup(t *testing.T) {
	t.Helper()
	select {
	case <-p.waitDone:
		return
	default:
	}
	_ = p.stdin.Close()
	select {
	case <-p.waitDone:
		return
	case <-time.After(time.Second):
	}
	if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("kill Pi process: %v", err)
	}
	select {
	case <-p.waitDone:
	case <-time.After(processTimeout):
		t.Errorf("Pi process was not reaped; stderr:\n%s", p.stderr.String())
	}
}

func requireM1ContentOnlyRequest(t *testing.T, environment *m1PiEnvironment, request m1ModelRequest) {
	t.Helper()
	require.Equal(t, http.MethodPost, request.Method)
	require.Equal(t, "/v1/chat/completions", request.Path)
	require.Equal(t, "application/json", request.Header.Get("Content-Type"))
	require.Equal(t, "Bearer m1-placeholder-key", request.Authorization)
	require.JSONEq(t, `"agent-whiteboard-m1"`, string(request.Fields["model"]))
	require.JSONEq(t, `true`, string(request.Fields["stream"]))
	require.NotEmpty(t, request.Fields["messages"])
	if tools, present := request.Fields["tools"]; present {
		require.JSONEq(t, `[]`, string(tools), "model request must not expose any callable tool definitions: %s", request.Body)
	}
	require.NotContains(t, string(request.Body), environment.configDir)
	require.NotContains(t, string(request.Body), environment.sessionDir)
	require.NotContains(t, string(request.Body), "pi-coding-agent")
	require.NotContains(t, string(request.Body), "M1_PROJECT_RESOURCE_MUST_NOT_REACH_MODEL")
	require.NotContains(t, string(request.Body), "M1_EXTENSION_SIDE_EFFECT")
}

func TestM1PiStartupStreamingPersistenceResumeAndCleanShutdown(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "host-openai-key-must-not-be-inherited")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "host-auth-token-must-not-be-inherited")
	t.Setenv("PI_CODING_AGENT_DIR", "/host/pi/config/must-not-be-used")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "/host/pi/sessions/must-not-be-used")
	model := newM1ModelServer(t)
	environment := newM1PiEnvironment(t, model.URL())
	environment.requireIsolated(t)

	model.enqueue(m1ModelScript{kind: m1StreamText, chunks: []string{"hello ", "from pinned Pi"}})
	first := startM1Pi(t, environment, "")
	commands := first.command(t, "m1-commands", "get_commands", nil)
	encodedCommands, err := json.Marshal(commands["commands"])
	require.NoError(t, err)
	require.NotContains(t, string(encodedCommands), environment.workspace)
	require.NotContains(t, string(encodedCommands), "M1_PROJECT_RESOURCE_MUST_NOT_REACH_MODEL")
	firstResult := first.prompt(t, "m1-prompt-1", "first reader message")
	require.Equal(t, "hello from pinned Pi", firstResult.text)
	require.Equal(t, []string{"hello ", "from pinned Pi"}, firstResult.deltas)
	request := model.waitRequest(t)
	requireM1ContentOnlyRequest(t, environment, request)
	state := first.command(t, "m1-state", "get_state", nil)
	sessionFile, ok := state["sessionFile"].(string)
	require.True(t, ok)
	require.FileExists(t, sessionFile)
	relativeSession, err := filepath.Rel(environment.sessionDir, sessionFile)
	require.NoError(t, err)
	require.True(t, filepath.IsLocal(relativeSession), "session escaped isolated directory: %s", sessionFile)
	first.stop(t)

	model.enqueue(m1ModelScript{kind: m1StreamText, chunks: []string{"resumed ", "successfully"}})
	resumed := startM1Pi(t, environment, sessionFile)
	messages := resumed.command(t, "m1-messages", "get_messages", nil)
	encodedMessages, err := json.Marshal(messages["messages"])
	require.NoError(t, err)
	require.Contains(t, string(encodedMessages), "first reader message")
	require.Contains(t, string(encodedMessages), "hello from pinned Pi")
	resumedResult := resumed.prompt(t, "m1-prompt-2", "second reader message")
	require.Equal(t, "resumed successfully", resumedResult.text)
	require.Equal(t, []string{"resumed ", "successfully"}, resumedResult.deltas)
	resumedRequest := model.waitRequest(t)
	requireM1ContentOnlyRequest(t, environment, resumedRequest)
	require.Contains(t, string(resumedRequest.Body), "first reader message")
	require.Contains(t, string(resumedRequest.Body), "hello from pinned Pi")
	resumed.stop(t)

	require.NoFileExists(t, filepath.Join(environment.workspace, "M1_EXTENSION_SIDE_EFFECT"))
}

func TestM1PiAbortInterruptsWithoutAutomaticReplay(t *testing.T) {
	model := newM1ModelServer(t)
	environment := newM1PiEnvironment(t, model.URL())
	model.enqueue(m1ModelScript{kind: m1WaitForAbort})
	process := startM1Pi(t, environment, "")

	process.send(t, map[string]any{"id": "m1-delayed", "type": "prompt", "message": "delayed reader message"})
	request := model.waitRequest(t)
	requireM1ContentOnlyRequest(t, environment, request)
	abortRecords := process.abortAndWaitSettled(t, "m1-abort")
	encodedAbort, err := json.Marshal(abortRecords)
	require.NoError(t, err)
	require.Contains(t, string(encodedAbort), "aborted")
	model.waitAbort(t)
	state := process.command(t, "m1-after-abort", "get_state", nil)
	require.Equal(t, false, state["isStreaming"])
	model.requireNoRequest(t, 500*time.Millisecond)
	process.stop(t)
}

func TestM1PiModelErrorIsNotAutomaticallyReplayed(t *testing.T) {
	model := newM1ModelServer(t)
	environment := newM1PiEnvironment(t, model.URL())
	model.enqueue(m1ModelScript{kind: m1ReturnError})
	process := startM1Pi(t, environment, "")

	errorResult := process.prompt(t, "m1-error", "error reader message")
	encodedError, err := json.Marshal(errorResult.records)
	require.NoError(t, err)
	require.Contains(t, string(encodedError), "scripted model error")
	request := model.waitRequest(t)
	requireM1ContentOnlyRequest(t, environment, request)
	model.requireNoRequest(t, 500*time.Millisecond)
	process.stop(t)
}

func TestM1PiMaliciousToolAttemptHasNoAuthorityOrSideEffect(t *testing.T) {
	model := newM1ModelServer(t)
	environment := newM1PiEnvironment(t, model.URL())
	model.enqueue(m1ModelScript{kind: m1AttemptToolCall})
	model.enqueue(m1ModelScript{kind: m1StreamText, chunks: []string{"continued without tool authority"}})
	process := startM1Pi(t, environment, "")

	result := process.prompt(t, "m1-tool-attempt", "attempt a malicious tool")
	require.Equal(t, "continued without tool authority", result.text)

	var completedCall, rejectedTool m1RPCRecord
	for _, record := range result.records {
		if record["type"] == "message_update" {
			update, _ := record["assistantMessageEvent"].(map[string]any)
			if update["type"] == "toolcall_end" {
				completedCall = record
			}
		}
		if record["type"] == "tool_execution_end" && record["toolCallId"] == "call_malicious" {
			rejectedTool = record
		}
	}
	encodedCall, err := json.Marshal(completedCall)
	require.NoError(t, err)
	require.Contains(t, string(encodedCall), "call_malicious")
	require.Contains(t, string(encodedCall), "M1_MALICIOUS_SIDE_EFFECT")
	require.NotNil(t, rejectedTool, "completed tool call did not reach Pi's rejection boundary")
	require.Equal(t, "bash", rejectedTool["toolName"])
	require.Equal(t, true, rejectedTool["isError"])
	encodedRejection, err := json.Marshal(rejectedTool)
	require.NoError(t, err)
	require.Contains(t, string(encodedRejection), "Tool bash not found")

	firstRequest := model.waitRequest(t)
	continuationRequest := model.waitRequest(t)
	requireM1ContentOnlyRequest(t, environment, firstRequest)
	require.NotContains(t, firstRequest.Fields, "tools", "initial model request must omit the tools field")
	requireM1ContentOnlyRequest(t, environment, continuationRequest)
	require.JSONEq(t, `[]`, string(continuationRequest.Fields["tools"]), "tool-rejection continuation must expose no callable tools")
	require.Contains(t, string(continuationRequest.Body), "Tool bash not found")
	require.Contains(t, string(continuationRequest.Body), "M1_MALICIOUS_SIDE_EFFECT")
	model.requireNoRequest(t, 500*time.Millisecond)
	require.NoFileExists(t, filepath.Join(environment.workspace, "M1_MALICIOUS_SIDE_EFFECT"))
	require.NoFileExists(t, filepath.Join(environment.workspace, "M1_EXTENSION_SIDE_EFFECT"))
	process.stop(t)
}

func TestPiAdapterRealCLILifecycleThroughProviderContract(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), processTimeout)
	defer cancel()
	model := newM1ModelServer(t)
	environment := newM1PiEnvironment(t, model.URL())
	environment.requireIsolated(t)
	workspace, err := filepath.EvalSymlinks(environment.workspace)
	require.NoError(t, err)
	home, err := filepath.EvalSymlinks(environment.home)
	require.NoError(t, err)
	providerRoot := filepath.Join(home, "provider")
	require.NoError(t, os.Mkdir(providerRoot, 0o700))
	driver, err := piadapter.NewDriver(piadapter.Config{
		Executable: m1PinnedPiPath(t), Environment: environment.env, ProviderRoot: providerRoot,
		Launcher: processgroup.NewLauncher(), IDs: common.CryptoIDGenerator{}, Clock: common.SystemClock{},
	})
	require.NoError(t, err)
	require.Equal(t, provider.Ready, driver.Readiness(ctx).State)

	session, err := driver.Create(ctx, provider.CreateRequest{Provider: provider.NamePi, Access: provider.AccessContentOnly, Workspace: workspace})
	require.NoError(t, err)
	require.NoError(t, session.NativeSession().Validate())
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), processTimeout)
		defer cleanupCancel()
		_ = session.Shutdown(cleanupCtx)
		_ = driver.Delete(cleanupCtx, provider.DeleteRequest{Provider: provider.NamePi, NativeSession: session.NativeSession().Ref})
	})
	turnID, err := (common.CryptoIDGenerator{}).NewID()
	require.NoError(t, err)
	messageID, err := (common.CryptoIDGenerator{}).NewID()
	require.NoError(t, err)
	resourceID, err := (common.CryptoIDGenerator{}).NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	markdown := []byte("# Adapter integration\nExact context marker.")
	creator := []byte("Creator context marker.")
	turn := provider.TurnRequest{
		TurnID: turnID, MessageID: messageID, Message: "Answer from this exact board.",
		Context: &provider.PageContext{
			Revision: provider.ContextInitial, Markdown: markdown, CreatorContext: creator,
			Title: "Adapter board", URL: "https://example.test/adapter", Digest: contextdigest.Calculate(markdown, creator),
			Resource: provider.Resource{Kind: provider.ResourceMarkdown, ID: resourceID, CreatedAt: now, UpdatedAt: now},
		},
	}
	preflight, err := session.Preflight(ctx, provider.PreflightRequest{Turn: turn})
	require.NoError(t, err)
	require.NoError(t, preflight.Validate())

	model.enqueue(m1ModelScript{kind: m1StreamText, chunks: []string{"adapter ", "answer"}})
	accepted, err := session.Submit(ctx, turn)
	require.NoError(t, err)
	require.NoError(t, accepted.Validate())
	var delta strings.Builder
	var userText, assistantText string
	var userCount, assistantCount, completionCount int
	for {
		select {
		case event, open := <-session.Events():
			require.True(t, open)
			require.NoError(t, event.Validate())
			switch event.Kind {
			case provider.EventUserMessage:
				userCount++
				userText = event.Text
			case provider.EventAssistantDelta:
				delta.WriteString(event.Text)
			case provider.EventAssistantMessage:
				assistantCount++
				assistantText = event.Text
			case provider.EventCompletion:
				completionCount++
				goto settled
			case provider.EventTerminalFailure:
				require.FailNow(t, "unexpected adapter terminal failure", "%s", event.Failure.Error())
			case provider.EventBlocked, provider.EventInterruption:
				require.FailNow(t, "unexpected adapter terminal event", "%s", event.Kind)
			}
			require.NotContains(t, event.Text, "agent-whiteboard-turn-v1")
			require.NotContains(t, event.Text, environment.configDir)
		case <-ctx.Done():
			require.FailNow(t, "adapter turn did not settle", "%v", ctx.Err())
		}
	}

settled:
	require.Equal(t, 1, userCount)
	require.Equal(t, 1, assistantCount)
	require.Equal(t, 1, completionCount)
	require.Equal(t, turn.Message, userText)
	require.Equal(t, "adapter answer", delta.String())
	require.Equal(t, "adapter answer", assistantText)
	request := model.waitRequest(t)
	requireM1ContentOnlyRequest(t, environment, request)
	expectedEnvelope, err := piadapter.BuildEnvelope(turn)
	require.NoError(t, err)
	var requestMessages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	require.NoError(t, json.Unmarshal(request.Fields["messages"], &requestMessages))
	exactPrompt := false
	for _, message := range requestMessages {
		if message.Role != "user" {
			continue
		}
		var text string
		if json.Unmarshal(message.Content, &text) != nil {
			var parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(message.Content, &parts) == nil {
				var combined strings.Builder
				for _, part := range parts {
					if part.Type == "text" {
						combined.WriteString(part.Text)
					}
				}
				text = combined.String()
			}
		}
		if text == string(expectedEnvelope) {
			exactPrompt = true
		}
	}
	require.True(t, exactPrompt, "model request did not contain the exact canonical envelope")

	state, err := session.Reconcile(ctx, provider.TurnReference{TurnID: turnID})
	require.NoError(t, err)
	require.Equal(t, provider.TurnCompleted, state)
	history, err := session.History(ctx, provider.HistoryRequest{Limit: 10})
	require.NoError(t, err)
	require.NoError(t, history.Validate())
	require.Len(t, history.Items, 2)
	require.Equal(t, provider.HistoryAssistant, history.Items[0].Role)
	require.Equal(t, "adapter answer", history.Items[0].Text)
	require.Equal(t, provider.HistoryUser, history.Items[1].Role)
	require.Equal(t, turn.Message, history.Items[1].Text)

	native := session.NativeSession()
	require.NoError(t, session.Shutdown(ctx))
	resumed, err := driver.Resume(ctx, provider.ResumeRequest{Provider: provider.NamePi, Access: provider.AccessContentOnly, NativeSession: native.Ref, Workspace: workspace})
	require.NoError(t, err)
	require.Equal(t, native.Ref, resumed.NativeSession().Ref)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), processTimeout)
		defer cleanupCancel()
		_ = resumed.Shutdown(cleanupCtx)
	})
	resumedHistory, err := resumed.History(ctx, provider.HistoryRequest{Limit: 10})
	require.NoError(t, err)
	require.Equal(t, history.Items, resumedHistory.Items)
	require.NoError(t, resumed.Shutdown(ctx))
	require.NoError(t, driver.Delete(ctx, provider.DeleteRequest{Provider: provider.NamePi, NativeSession: native.Ref}))
	_, err = driver.Inspect(ctx, provider.InspectRequest{Provider: provider.NamePi, NativeSession: native.Ref})
	var missing provider.ProviderError
	require.ErrorAs(t, err, &missing)
	require.Equal(t, provider.ErrorNativeSessionMissing, missing.Code())
	require.NoFileExists(t, filepath.Join(environment.workspace, "M1_EXTENSION_SIDE_EFFECT"))
}
