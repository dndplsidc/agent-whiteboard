package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProcessHealth(t *testing.T) {
	server := startServer(t)
	requireStatus(t, server.URL+"/healthz", http.StatusOK)
	requireStatus(t, server.URL+"/readyz", http.StatusOK)
}

func TestProcessClientConfigurationPrecedence(t *testing.T) {
	server := startServer(t)
	created := requestMarkdownPair(t, http.MethodPost, server.URL+"/api/v1/whiteboards/markdown", []byte("# precedence\n"), []byte("# context\n"), http.StatusCreated)
	configPath := writeConfigFixture(t, "version: 1\nclient:\n  server: http://127.0.0.1:1\n  timeout: 5s\n")

	for _, test := range []struct {
		name string
		env  []string
		args []string
	}{
		{
			name: "environment overrides YAML",
			env:  append(append([]string{}, server.env...), "AGENT_WHITEBOARD_SERVER="+server.URL),
			args: []string{"--config", configPath, "--json", "get", "markdown", "--", created.Resource.ID},
		},
		{
			name: "flag overrides environment and YAML",
			env:  append(append([]string{}, server.env...), "AGENT_WHITEBOARD_SERVER=http://127.0.0.1:2"),
			args: []string{"--config", configPath, "--server", server.URL, "--json", "get", "markdown", "--", created.Resource.ID},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
			defer cancel()
			stdout, stderr, err := server.RunCLIWithEnv(ctx, test.env, test.args...)
			require.NoError(t, err, stderr)
			require.Empty(t, stderr)
			require.True(t, json.Valid([]byte(stdout)), "stdout must contain JSON only: %q", stdout)
			var got cliMarkdownEnvelope
			require.NoError(t, json.Unmarshal([]byte(stdout), &got))
			require.Equal(t, "# precedence\n", got.Markdown)
			require.Equal(t, "# context\n", got.Context)
		})
	}
}

func TestProcessIgnoresInheritedConfiguration(t *testing.T) {
	t.Setenv("AGENT_WHITEBOARD_SHUTDOWN_TIMEOUT", "invalid")
	t.Setenv("agent_whiteboard_max_whiteboard_bytes", "invalid")

	server := startServer(t)
	requireStatus(t, server.URL+"/readyz", http.StatusOK)
	for _, entry := range server.env {
		key, _, _ := strings.Cut(entry, "=")
		require.False(t, strings.HasPrefix(strings.ToUpper(key), "AGENT_WHITEBOARD_"), entry)
	}
}

func TestServerLogWriterCapturesChunkedLogAndTail(t *testing.T) {
	output := &lockedBuffer{}
	logs := make(chan serverLog, 1)
	writer := &serverLogWriter{output: output, logs: logs}
	writes := []string{
		`{"level":"INFO","msg":"server`,
		` listening","address":"127.0.0.1:1234","url":"http://127.0.0.1:1234"}` + "\npartial",
		" tail without newline",
	}
	for _, chunk := range writes {
		_, err := writer.Write([]byte(chunk))
		require.NoError(t, err)
	}

	require.Equal(t, strings.Join(writes, ""), output.String())
	select {
	case entry := <-logs:
		require.Equal(t, "server listening", entry.Message)
		require.Equal(t, "127.0.0.1:1234", entry.Address)
	case <-time.After(time.Second):
		require.FailNow(t, "structured listening log was not emitted")
	}
}
