package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/app"
	"github.com/dndplsidc/agent-whiteboard/internal/common"
	"github.com/stretchr/testify/require"
)

func TestServeBuildsExactApplicationAndOwnsLifecycle(t *testing.T) {
	storage := t.TempDir()
	settings := resolvedServerSettings{
		host: "flag.host", port: 0, storage: storage,
		cleanupInterval: 2 * time.Minute, defaultExpiration: 0,
		shutdownTimeout: 3 * time.Second, logMode: "json",
		maxWhiteboardBytes: 11, maxContextBytes: 14, maxImageBytes: 12, maxImageRequestBytes: 13,
	}
	arguments := buildApplicationArguments(settings, io.Discard)
	require.Equal(t, 0, arguments.port)
	require.Equal(t, int64(0), arguments.defaultExpiration)
	require.Len(t, arguments.options(), 2)
	wantConfig := app.ServiceConfig{
		RootDir: storage, CleanupInterval: 2 * time.Minute,
		Host: "flag.host", ShutdownTimeout: 3 * time.Second,
		MaxWhiteboardBytes: 11, MaxContextBytes: 14, MaxImageBytes: 12, MaxImageRequestBytes: 13,
		LogMode: app.LogModeJSON,
	}

	contextKey := struct{}{}
	ctx := context.WithValue(context.Background(), contextKey, "present")
	application := &fakeApplication{listen: func(got context.Context) error {
		require.Same(t, ctx, got)
		require.Equal(t, "present", got.Value(contextKey))
		return nil
	}}
	deps := validDependencies()
	deps.NewApplication = func(config app.ServiceConfig, options ...app.Option) (Application, error) {
		require.NotNil(t, config.Logger)
		config.Logger = nil
		require.Equal(t, wantConfig, config)
		require.Len(t, options, 2)
		return application, nil
	}
	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{
		"serve", "--host", "flag.host", "--port", "0", "--storage", storage,
		"--cleanup-interval", "2m", "--default-expires-in", "0",
		"--shutdown-timeout", "3s", "--log-mode", "json",
		"--max-whiteboard-bytes", "11", "--max-context-bytes", "14", "--max-image-bytes", "12", "--max-image-request-bytes", "13",
	})
	require.NoError(t, root.ExecuteContext(ctx))
	require.Equal(t, int32(1), application.closeCalls.Load())
}

func TestServeConstructorFailureDoesNotClose(t *testing.T) {
	constructorErr := errors.New("construct failed")
	deps := validDependencies()
	deps.NewApplication = func(app.ServiceConfig, ...app.Option) (Application, error) {
		return nil, constructorErr
	}
	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"serve"})
	err = root.ExecuteContext(context.Background())
	require.ErrorIs(t, err, constructorErr)
}

func TestServeRejectsTypedNilApplication(t *testing.T) {
	var application *fakeApplication
	deps := validDependencies()
	deps.NewApplication = func(app.ServiceConfig, ...app.Option) (Application, error) {
		return application, nil
	}
	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"serve"})
	require.NotPanics(t, func() { err = root.ExecuteContext(context.Background()) })
	require.EqualError(t, err, "application factory returned nil")
	require.False(t, common.HasCode(err, common.CodeInvalidRequest), "error: %v", err)
}

func TestServeClosesAndJoinsPostConstructionErrors(t *testing.T) {
	listenErr := errors.New("listen failed")
	closeErr := errors.New("close failed")
	application := &fakeApplication{listen: func(context.Context) error { return listenErr }, closeErr: closeErr}
	deps := validDependencies()
	deps.NewApplication = func(app.ServiceConfig, ...app.Option) (Application, error) { return application, nil }
	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"serve"})
	err = root.ExecuteContext(context.Background())
	require.ErrorIs(t, err, listenErr)
	require.ErrorIs(t, err, closeErr)
	require.Equal(t, int32(1), application.closeCalls.Load())
}

func TestServeCancellationIsGracefulAndCloses(t *testing.T) {
	started := make(chan struct{})
	application := &fakeApplication{listen: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return fmt.Errorf("serve stopped: %w", ctx.Err())
	}}
	deps := validDependencies()
	deps.NewApplication = func(app.ServiceConfig, ...app.Option) (Application, error) { return application, nil }
	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"serve"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	<-started
	cancel()
	require.NoError(t, <-done)
	require.Equal(t, int32(1), application.closeCalls.Load())
}

func TestServeCancellationDoesNotSwallowJoinedFailure(t *testing.T) {
	started := make(chan struct{})
	listenErr := errors.New("listen failed")
	application := &fakeApplication{listen: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return errors.Join(fmt.Errorf("serve stopped: %w", ctx.Err()), listenErr)
	}}
	deps := validDependencies()
	deps.NewApplication = func(app.ServiceConfig, ...app.Option) (Application, error) { return application, nil }
	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"serve"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	<-started
	cancel()
	err = <-done
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, listenErr)
	require.Equal(t, int32(1), application.closeCalls.Load())
}

func TestApplicationLoggerWritesOnlyConfiguredStderrMode(t *testing.T) {
	for _, test := range []struct {
		name    string
		logMode string
		json    bool
	}{
		{name: "console", logMode: "console"},
		{name: "json", logMode: "json", json: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			deps := validDependencies()
			deps.Stdout, deps.Stderr = &stdout, &stderr
			deps.NewApplication = func(config app.ServiceConfig, _ ...app.Option) (Application, error) {
				config.Logger.Info("lifecycle probe", "component", "server")
				return &fakeApplication{}, nil
			}
			root, err := NewRoot(deps)
			require.NoError(t, err)
			root.SetArgs([]string{"serve", "--log-mode", test.logMode})
			require.NoError(t, root.ExecuteContext(context.Background()))
			require.Empty(t, stdout.String())
			if test.json {
				var entry map[string]any
				require.NoError(t, json.Unmarshal(stderr.Bytes(), &entry))
				require.Equal(t, "lifecycle probe", entry["msg"])
				require.Equal(t, "server", entry["component"])
			} else {
				require.Contains(t, stderr.String(), "msg=\"lifecycle probe\"")
				require.Contains(t, stderr.String(), "component=server")
			}
			require.NotContains(t, stderr.String(), "secret")
		})
	}
}

type fakeApplication struct {
	listen     func(context.Context) error
	closeErr   error
	closeCalls atomic.Int32
}

func (application *fakeApplication) ListenAndServe(ctx context.Context) error {
	if application.listen == nil {
		return nil
	}
	return application.listen(ctx)
}

func (application *fakeApplication) Close() error {
	application.closeCalls.Add(1)
	return application.closeErr
}
