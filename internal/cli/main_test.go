package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/edocsss/agent-whiteboard/internal/app"
	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/testutil"
	httpx "github.com/edocsss/agent-whiteboard/internal/webapi"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestRunPrintsHelpSuccessfully(t *testing.T) {
	originalArgs := os.Args
	os.Args = []string{"agent-whiteboard"}
	t.Cleanup(func() { os.Args = originalArgs })
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), &stdout, &stderr, mapGetenv(nil))
	require.Zero(t, code)
	require.Contains(t, stdout.String(), "Usage:")
	require.Empty(t, stderr.String())
}

func TestRunWithDependenciesExitMappingAndOutput(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		clientErr  error
		wantCode   int
		wantStderr string
	}{
		{name: "local configuration", args: []string{"--server", "invalid", "image", "delete", "abc"}, wantCode: 2, wantStderr: "Error: server must be an absolute HTTP origin\n"},
		{name: "argument usage", args: []string{"image", "delete"}, wantCode: 2, wantStderr: "Error: accepts 1 arg(s), received 0\n"},
		{name: "unknown command", args: []string{"unknown"}, wantCode: 2, wantStderr: "Error: unknown command \"unknown\" for \"agent-whiteboard\"\n"},
		{name: "unknown flag", args: []string{"--unknown"}, wantCode: 2, wantStderr: "Error: unknown flag: --unknown\n"},
		{name: "argument usage json", args: []string{"--json", "image", "delete"}, wantCode: 2, wantStderr: "{\"schema_version\":1,\"error\":{\"code\":\"invalid_request\",\"message\":\"accepts 1 arg(s), received 0\"}}\n"},
		{name: "early json uppercase", args: []string{"--json=TRUE", "unknown"}, wantCode: 2, wantStderr: "{\"schema_version\":1,\"error\":{\"code\":\"invalid_request\",\"message\":\"unknown command \\\"unknown\\\" for \\\"agent-whiteboard\\\"\"}}\n"},
		{name: "early json numeric", args: []string{"--json=1", "unknown"}, wantCode: 2, wantStderr: "{\"schema_version\":1,\"error\":{\"code\":\"invalid_request\",\"message\":\"unknown command \\\"unknown\\\" for \\\"agent-whiteboard\\\"\"}}\n"},
		{name: "early json false", args: []string{"--json=false", "unknown"}, wantCode: 2, wantStderr: "Error: unknown command \"unknown\" for \"agent-whiteboard\"\n"},
		{name: "remote invalid request", args: []string{"image", "delete", "abc"}, clientErr: common.NewError(common.CodeInvalidRequest, "remote rejected", nil), wantCode: 3, wantStderr: "Error: remote rejected\n"},
		{name: "remote not found", args: []string{"image", "delete", "abc"}, clientErr: common.NewError(common.CodeNotFound, "resource not found", nil), wantCode: 3, wantStderr: "Error: resource not found\n"},
		{name: "unexpected", args: []string{"image", "delete", "abc"}, clientErr: errors.New("private detail"), wantCode: 1, wantStderr: "Error: internal error\n"},
		{name: "deadline json", args: []string{"--json", "image", "delete", "abc"}, clientErr: context.DeadlineExceeded, wantCode: 4, wantStderr: "{\"schema_version\":1,\"error\":{\"code\":\"timeout\",\"message\":\"request timed out\"}}\n"},
		{name: "canceled", args: []string{"image", "delete", "abc"}, clientErr: context.Canceled, wantCode: 4, wantStderr: "Error: request canceled\n"},
		{name: "wrapped joined contexts", args: []string{"image", "delete", "abc"}, clientErr: errors.Join(fmt.Errorf("wrapped: %w", context.Canceled), fmt.Errorf("wrapped: %w", context.DeadlineExceeded)), wantCode: 4, wantStderr: "Error: request timed out\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := testutil.NewMockClient(t)
			if test.clientErr != nil {
				client.EXPECT().DeleteImage(mock.Anything, "abc").Return(test.clientErr).Once()
			}
			deps := validDependencies()
			deps.NewClient = func(httpx.ClientConfig) (Client, error) { return client, nil }
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), &stdout, &stderr, mapGetenv(nil), test.args, deps)
			require.Equal(t, test.wantCode, code)
			require.Empty(t, stdout.String())
			require.Equal(t, test.wantStderr, stderr.String())
		})
	}
}

func TestRunGracefulCanceledServeReturnsZero(t *testing.T) {
	application := &fakeApplication{listen: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	deps := validDependencies()
	deps.NewApplication = func(app.ServiceConfig, ...app.Option) (Application, error) { return application, nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	code := run(ctx, &stdout, &stderr, mapGetenv(nil), []string{"serve"}, deps)
	require.Zero(t, code)
	require.Empty(t, stdout.String())
	require.Empty(t, stderr.String())
}

func TestRunMapsTypedNilFactoryResultsToInternal(t *testing.T) {
	t.Run("client", func(t *testing.T) {
		var client *testutil.MockClient
		deps := validDependencies()
		deps.NewClient = func(httpx.ClientConfig) (Client, error) { return client, nil }
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), &stdout, &stderr, mapGetenv(nil), []string{"image", "delete", "abc"}, deps)
		require.Equal(t, exitInternal, code)
		require.Empty(t, stdout.String())
		require.Equal(t, "Error: internal error\n", stderr.String())
	})

	t.Run("application", func(t *testing.T) {
		var application *fakeApplication
		deps := validDependencies()
		deps.NewApplication = func(app.ServiceConfig, ...app.Option) (Application, error) { return application, nil }
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), &stdout, &stderr, mapGetenv(nil), []string{"serve"}, deps)
		require.Equal(t, exitInternal, code)
		require.Empty(t, stdout.String())
		require.Equal(t, "Error: internal error\n", stderr.String())
	})
}

func TestRunNilProcessDependenciesDoNotPanic(t *testing.T) {
	originalArgs := os.Args
	os.Args = []string{"agent-whiteboard"}
	t.Cleanup(func() { os.Args = originalArgs })
	tests := []struct {
		name   string
		stdout io.Writer
		stderr io.Writer
		getenv func(string) string
	}{
		{name: "stdout", stderr: io.Discard, getenv: mapGetenv(nil)},
		{name: "stderr", stdout: io.Discard, getenv: mapGetenv(nil)},
		{name: "getenv", stdout: io.Discard, stderr: io.Discard},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var code int
			require.NotPanics(t, func() { code = Run(context.Background(), test.stdout, test.stderr, test.getenv) })
			require.Equal(t, exitUsage, code)
		})
	}
}

func TestRunDoesNotHideFailuresJoinedWithCancellation(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		failure    error
		wantCode   int
		wantStderr string
	}{
		{name: "plain human", args: []string{"serve"}, failure: errors.New("shutdown failed"), wantCode: exitInternal, wantStderr: "Error: internal error\n"},
		{name: "plain json", args: []string{"--json", "serve"}, failure: errors.New("shutdown failed"), wantCode: exitInternal, wantStderr: "{\"schema_version\":1,\"error\":{\"code\":\"internal_error\",\"message\":\"internal error\"}}\n"},
		{name: "domain", args: []string{"serve"}, failure: common.NewError(common.CodeStorageUnavailable, "storage unavailable", nil), wantCode: exitRemote, wantStderr: "Error: storage unavailable\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			application := &fakeApplication{listen: func(ctx context.Context) error {
				return errors.Join(ctx.Err(), test.failure)
			}}
			deps := validDependencies()
			deps.NewApplication = func(app.ServiceConfig, ...app.Option) (Application, error) { return application, nil }
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var stdout, stderr bytes.Buffer
			code := run(ctx, &stdout, &stderr, mapGetenv(nil), test.args, deps)
			require.Equal(t, test.wantCode, code)
			require.Empty(t, stdout.String())
			require.Equal(t, test.wantStderr, stderr.String())
		})
	}
}

func TestRequestedJSONMatchesBooleanFlagSpellings(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want bool
	}{
		{name: "bare", args: []string{"--json"}, want: true},
		{name: "uppercase", args: []string{"--json=TRUE"}, want: true},
		{name: "numeric", args: []string{"--json=1"}, want: true},
		{name: "short", args: []string{"--json=t"}, want: true},
		{name: "false", args: []string{"--json=false"}},
		{name: "last wins false", args: []string{"--json", "--json=0"}},
		{name: "last wins true", args: []string{"--json=false", "--json=1"}, want: true},
		{name: "after terminator ignored", args: []string{"--", "--json"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, requestedJSON(test.args))
		})
	}
}
