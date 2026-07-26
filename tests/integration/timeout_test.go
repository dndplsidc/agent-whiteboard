package integration

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClientTimeoutHumanAndJSON(t *testing.T) {
	for _, test := range []struct {
		name       string
		json       bool
		wantStderr string
	}{
		{name: "human", wantStderr: "Error: request timed out\n"},
		{name: "JSON", json: true, wantStderr: "{\"schema_version\":1,\"error\":{\"code\":\"timeout\",\"message\":\"request timed out\"}}\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			requestCanceled := make(chan struct{}, 1)
			releaseHandler := make(chan struct{})
			delayed := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
				_, _ = io.Copy(io.Discard, request.Body)
				_ = request.Body.Close()
				select {
				case <-request.Context().Done():
					requestCanceled <- struct{}{}
				case <-releaseHandler:
				}
			}))
			t.Cleanup(delayed.Close)
			t.Cleanup(func() { close(releaseHandler) })
			file := writeFixture(t, "timeout.md", []byte("# timeout\n"))
			creatorContext := writeContextFixture(t, "# Timeout context\n")
			args := []string{"--server", delayed.URL, "--timeout", "20ms"}
			if test.json {
				args = append(args, "--json")
			}
			args = append(args, "create", "markdown", "--context", creatorContext, file)

			ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
			defer cancel()
			command := exec.CommandContext(ctx, binaryPath, args...)
			command.Env = isolatedEnv(t.TempDir())
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			command.Stdout = &stdout
			command.Stderr = &stderr
			err := command.Run()
			require.Error(t, err)
			var exitError *exec.ExitError
			require.ErrorAs(t, err, &exitError)
			require.Equal(t, 4, exitError.ExitCode())
			require.Empty(t, stdout.String())
			require.Equal(t, test.wantStderr, stderr.String())

			timer := time.NewTimer(integrationTimeout)
			defer timer.Stop()
			select {
			case <-requestCanceled:
			case <-timer.C:
				require.FailNow(t, "server request context was not canceled")
			}
		})
	}
}
