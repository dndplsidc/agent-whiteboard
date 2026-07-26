package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	processTimeout     = 10 * time.Second
	integrationTimeout = 10 * time.Second
	pollInterval       = 20 * time.Millisecond
)

var binaryPath string

func TestMain(m *testing.M) {
	buildDir, err := os.MkdirTemp("", "agent-whiteboard-integration-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create integration build directory: %v\n", err)
		os.Exit(1)
	}

	binaryPath, err = filepath.Abs(filepath.Join(buildDir, "agent-whiteboard"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve integration binary path: %v\n", err)
		_ = os.RemoveAll(buildDir)
		os.Exit(1)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	build := exec.Command("go", "build", "-trimpath", "-o", binaryPath, "../../cmd/agent-whiteboard")
	build.Stdout = &stdout
	build.Stderr = &stderr
	if err := build.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "build integration binary: %v\nstdout:\n%s\nstderr:\n%s\n", err, stdout.String(), stderr.String())
		_ = os.RemoveAll(buildDir)
		os.Exit(1)
	}

	code := m.Run()
	if err := os.RemoveAll(buildDir); err != nil {
		fmt.Fprintf(os.Stderr, "remove integration build directory: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type serverLog struct {
	Level   string `json:"level"`
	Message string `json:"msg"`
	Address string `json:"address"`
	URL     string `json:"url"`
}

type cliResource struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	ExpiresAt *int64 `json:"expires_at"`
	Permanent bool   `json:"permanent"`
}

type cliResourceEnvelope struct {
	SchemaVersion int         `json:"schema_version"`
	Resource      cliResource `json:"resource"`
}

type cliMarkdownEnvelope struct {
	SchemaVersion int         `json:"schema_version"`
	Resource      cliResource `json:"resource"`
	Markdown      string      `json:"markdown"`
	Context       string      `json:"context"`
}

type cliResourcesEnvelope struct {
	SchemaVersion int           `json:"schema_version"`
	Resources     []cliResource `json:"resources"`
}

type cliErrorEnvelope struct {
	SchemaVersion int `json:"schema_version"`
	Error         struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type serverLogWriter struct {
	mu      sync.Mutex
	output  *lockedBuffer
	pending []byte
	logs    chan<- serverLog
}

func (w *serverLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n, err := w.output.Write(p)
	if n == 0 {
		return n, err
	}
	w.pending = append(w.pending, p[:n]...)
	consumed := 0
	for {
		newline := bytes.IndexByte(w.pending[consumed:], '\n')
		if newline < 0 {
			break
		}
		newline += consumed
		var entry serverLog
		if json.Unmarshal(bytes.TrimSpace(w.pending[consumed:newline]), &entry) == nil && entry.Message == "server listening" {
			select {
			case w.logs <- entry:
			default:
			}
		}
		consumed = newline + 1
	}
	if consumed > 0 {
		copy(w.pending, w.pending[consumed:])
		w.pending = w.pending[:len(w.pending)-consumed]
	}
	return n, err
}

type testServer struct {
	Root    string
	URL     string
	Address string

	cmd      *exec.Cmd
	env      []string
	stdout   *lockedBuffer
	stderr   *lockedBuffer
	waitDone chan struct{}
	waitMu   sync.Mutex
	waitErr  error
}

func startServer(t *testing.T, extraArgs ...string) *testServer {
	t.Helper()

	root := t.TempDir()
	home := t.TempDir()
	env := isolatedEnv(home)
	args := []string{
		"serve",
		"--host", "127.0.0.1",
		"--port", "0",
		"--storage", root,
		"--log-mode", "json",
	}
	args = append(args, extraArgs...)

	cmd := exec.Command(binaryPath, args...)
	cmd.Env = env
	logs := make(chan serverLog, 1)
	server := &testServer{
		Root:     root,
		cmd:      cmd,
		env:      env,
		stdout:   &lockedBuffer{},
		stderr:   &lockedBuffer{},
		waitDone: make(chan struct{}),
	}
	cmd.Stdout = server.stdout
	cmd.Stderr = &serverLogWriter{output: server.stderr, logs: logs}

	require.NoError(t, cmd.Start(), "start server process")
	t.Cleanup(func() {
		server.cleanup(t)
	})

	go server.reap()

	server.Address, server.URL = waitForListeningLog(t, server, logs)
	waitForReady(t, server.URL+"/readyz")
	return server
}

func isolatedEnv(home string) []string {
	env := make([]string, 0, len(os.Environ())+3)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		upperKey := strings.ToUpper(key)
		if strings.HasPrefix(upperKey, "AGENT_WHITEBOARD_") {
			continue
		}
		switch upperKey {
		case "HOME", "USERPROFILE", "XDG_CONFIG_HOME":
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"HOME="+home,
		"USERPROFILE="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
	)
}

func (s *testServer) reap() {
	err := s.cmd.Wait()
	s.waitMu.Lock()
	s.waitErr = err
	s.waitMu.Unlock()
	close(s.waitDone)
}

func waitForListeningLog(t *testing.T, server *testServer, logs <-chan serverLog) (string, string) {
	t.Helper()
	timer := time.NewTimer(processTimeout)
	defer timer.Stop()

	for {
		select {
		case entry := <-logs:
			if entry.Message != "server listening" {
				continue
			}
			require.NotEmpty(t, entry.Address, "server listening log must contain address")
			parsed, err := url.Parse(entry.URL)
			require.NoError(t, err, "parse listening URL")
			require.Contains(t, []string{"http", "https"}, parsed.Scheme)
			require.NotEmpty(t, parsed.Host, "server listening log must contain a usable URL")
			return entry.Address, entry.URL
		case <-server.waitDone:
			require.FailNow(t, "server exited before listening", "stdout:\n%s\nstderr:\n%s", server.Stdout(), server.Stderr())
		case <-timer.C:
			require.FailNow(t, "timed out waiting for server listening log", "stdout:\n%s\nstderr:\n%s", server.Stdout(), server.Stderr())
		}
	}
}

func waitForReady(t *testing.T, endpoint string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), processTimeout)
	defer cancel()
	client := &http.Client{Timeout: 500 * time.Millisecond}

	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		require.NoError(t, err)
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}

		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			require.FailNow(t, "server did not become ready", "endpoint: %s; error: %v", endpoint, ctx.Err())
		case <-timer.C:
		}
	}
}

func requireStatus(t *testing.T, endpoint string, status int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), processTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	require.NoError(t, err)
	response, err := (&http.Client{Timeout: processTimeout}).Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, status, response.StatusCode)
}

func requireUnavailable(t *testing.T, endpoint string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), processTimeout)
	defer cancel()
	client := &http.Client{Timeout: 500 * time.Millisecond}

	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		require.NoError(t, err)
		response, err := client.Do(request)
		if err != nil {
			return
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return
		}

		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			require.FailNow(t, "server remained ready", "endpoint: %s", endpoint)
		case <-timer.C:
		}
	}
}

func writeFixture(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, content, 0o600))
	return path
}

func writeContextFixture(t *testing.T, content string) string {
	t.Helper()
	return writeFixture(t, "context.md", []byte(content))
}

func writeConfigFixture(t *testing.T, content string) string {
	t.Helper()
	return writeFixture(t, "config.yaml", []byte(content))
}

func fetch(t *testing.T, endpoint string) (*http.Response, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	require.NoError(t, err)
	response, err := (&http.Client{Timeout: integrationTimeout}).Do(request)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	return response, string(body)
}

func runCLIResource(t *testing.T, server *testServer, args ...string) cliResourceEnvelope {
	t.Helper()
	stdout := runCLISuccess(t, server, args...)
	var envelope cliResourceEnvelope
	require.NoError(t, json.Unmarshal([]byte(stdout), &envelope), stdout)
	return envelope
}

func runCLIWithConfigSuccess(t *testing.T, server *testServer, configPath string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()
	stdout, stderr, err := server.RunCLIWithConfig(ctx, configPath, args...)
	require.NoError(t, err, "stderr: %s", stderr)
	require.Empty(t, stderr)
	require.True(t, json.Valid([]byte(stdout)), "stdout must contain JSON only: %q", stdout)
	return stdout
}

func runCLIResourceWithConfig(t *testing.T, server *testServer, configPath string, args ...string) cliResourceEnvelope {
	t.Helper()
	var envelope cliResourceEnvelope
	stdout := runCLIWithConfigSuccess(t, server, configPath, args...)
	require.NoError(t, json.Unmarshal([]byte(stdout), &envelope), stdout)
	return envelope
}

func runCLIMarkdownWithConfig(t *testing.T, server *testServer, configPath string, args ...string) cliMarkdownEnvelope {
	t.Helper()
	var envelope cliMarkdownEnvelope
	stdout := runCLIWithConfigSuccess(t, server, configPath, args...)
	require.NoError(t, json.Unmarshal([]byte(stdout), &envelope), stdout)
	return envelope
}

func runCLISuccess(t *testing.T, server *testServer, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()
	stdout, stderr, err := server.RunCLI(ctx, args...)
	require.NoError(t, err, "stderr: %s", stderr)
	require.Empty(t, stderr)
	return stdout
}

func runCLIDelete(t *testing.T, server *testServer, args ...string) {
	t.Helper()
	stdout := runCLISuccess(t, server, args...)
	require.JSONEq(t, `{"schema_version":1}`, stdout)
}

func requireJSONError(t *testing.T, value, code string) cliErrorEnvelope {
	t.Helper()
	var envelope cliErrorEnvelope
	require.NoError(t, json.Unmarshal([]byte(value), &envelope), value)
	require.Equal(t, 1, envelope.SchemaVersion)
	require.Equal(t, code, envelope.Error.Code)
	require.NotEmpty(t, envelope.Error.Message)
	return envelope
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return encoded
}

func requireCategoryEmpty(t *testing.T, root, category string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, category))
	require.NoError(t, err)
	require.Empty(t, entries)
}

func (s *testServer) RunCLI(ctx context.Context, args ...string) (string, string, error) {
	commandArgs := append([]string{"--server", s.URL}, args...)
	return s.runCommand(ctx, s.env, commandArgs...)
}

func (s *testServer) RunCLIWithConfig(ctx context.Context, configPath string, args ...string) (string, string, error) {
	commandArgs := append([]string{"--config", configPath}, args...)
	return s.runCommand(ctx, s.env, commandArgs...)
}

func (s *testServer) RunCLIWithEnv(ctx context.Context, env []string, args ...string) (string, string, error) {
	return s.runCommand(ctx, env, args...)
}

func (s *testServer) runCommand(ctx context.Context, env []string, args ...string) (string, string, error) {
	command := exec.CommandContext(ctx, binaryPath, args...)
	command.Env = env
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

func (s *testServer) Signal(t *testing.T, signal os.Signal) {
	t.Helper()
	require.NoError(t, s.cmd.Process.Signal(signal))
}

func (s *testServer) Wait(t *testing.T) error {
	t.Helper()
	if err, ok := s.wait(processTimeout); ok {
		return err
	}
	return fmt.Errorf("timed out waiting for server process; stdout:\n%s\nstderr:\n%s", s.Stdout(), s.Stderr())
}

func (s *testServer) wait(timeout time.Duration) (error, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.waitDone:
		s.waitMu.Lock()
		defer s.waitMu.Unlock()
		return s.waitErr, true
	case <-timer.C:
		return nil, false
	}
}

func (s *testServer) exited() bool {
	select {
	case <-s.waitDone:
		return true
	default:
		return false
	}
}

func (s *testServer) cleanup(t *testing.T) {
	t.Helper()
	if !s.exited() {
		if err := s.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("force-kill server process: %v", err)
		}
	}
	if _, ok := s.wait(processTimeout); !ok {
		t.Errorf("server process was not reaped; stdout:\n%s\nstderr:\n%s", s.Stdout(), s.Stderr())
	}
}

func (s *testServer) Stdout() string {
	return s.stdout.String()
}

func (s *testServer) Stderr() string {
	return s.stderr.String()
}
