package app

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agentstate"
	"github.com/stretchr/testify/require"
)

func TestConfigTrustSourceReloadsAtomicReplacement(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	writeAgentConfig(t, path, "https://first.example")
	source := configTrustSource{selectedPath: path}

	origins, err := source.TrustedOrigins(context.Background())
	require.NoError(t, err)
	require.Equal(t, map[string]struct{}{"https://first.example": {}}, origins)

	replacement := filepath.Join(directory, "replacement.yaml")
	writeAgentConfig(t, replacement, "https://second.example")
	require.NoError(t, os.Rename(replacement, path))
	origins, err = source.TrustedOrigins(context.Background())
	require.NoError(t, err)
	require.Equal(t, map[string]struct{}{"https://second.example": {}}, origins)
}

func TestConfigTrustSourceHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (configTrustSource{}).TrustedOrigins(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestProviderEnvironmentIsExplicitAndOverrideIsCloned(t *testing.T) {
	t.Setenv("HOME", "/host/home")
	t.Setenv("USERPROFILE", "/host/profile")
	t.Setenv("PATH", "/safe/bin")
	t.Setenv("TMPDIR", "/tmp/safe")
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("LC_ALL", "C")
	t.Setenv("OPENAI_API_KEY", "host-secret")
	t.Setenv("HTTP_PROXY", "http://user:password@proxy.invalid")
	t.Setenv("AGENT_WHITEBOARD_AGENT_PORT", "host-control")

	environment, err := providerEnvironment("/isolated/home", nil)
	require.NoError(t, err)
	require.Contains(t, environment, "HOME=/isolated/home")
	require.Contains(t, environment, "USERPROFILE=/isolated/home")
	require.Contains(t, environment, "PATH=/safe/bin")
	require.NotContains(t, environment, "OPENAI_API_KEY=host-secret")
	require.NotContains(t, environment, "HTTP_PROXY=http://user:password@proxy.invalid")
	for _, entry := range environment {
		require.NotContains(t, entry, "AGENT_WHITEBOARD_AGENT_PORT")
	}

	override := []string{"HOME=/override", "TOKEN=kept"}
	cloned, err := providerEnvironment("/ignored", override)
	require.NoError(t, err)
	override[0] = "HOME=mutated"
	require.Equal(t, []string{"HOME=/override", "TOKEN=kept"}, cloned)
	require.NotNil(t, cloned)
	empty, err := providerEnvironment("/ignored", []string{})
	require.NoError(t, err)
	require.NotNil(t, empty)
}

func TestProviderEnvironmentRejectsDuplicateAndNUL(t *testing.T) {
	_, err := providerEnvironment("/home", []string{"A=1", "A=2"})
	require.ErrorContains(t, err, "duplicate")
	_, err = providerEnvironment("/home", []string{"A=bad\x00value"})
	require.ErrorContains(t, err, "NUL")
}

func TestNewAgentServiceFailureReleasesState(t *testing.T) {
	home := t.TempDir()
	executable := executableFixture(t)
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port

	service, err := NewAgentService(AgentServiceConfig{
		Home: home, Port: port, PiExecutable: executable, ProviderEnvironment: []string{},
		IdleTimeout: time.Second, ShutdownTimeout: time.Second,
	})
	require.Nil(t, service)
	require.Error(t, err)

	reopened, err := agentstate.Open(home)
	require.NoError(t, err)
	require.NoError(t, reopened.Close())
}

func TestAgentServiceEphemeralLifecycleAndIdempotentClose(t *testing.T) {
	home := t.TempDir()
	service, err := NewAgentService(AgentServiceConfig{
		Home: home, Port: 0, PiExecutable: executableFixture(t), ProviderEnvironment: []string{},
		IdleTimeout: time.Second, ShutdownTimeout: time.Second,
	})
	require.NoError(t, err)
	require.NotNil(t, service.Addr())
	require.Equal(t, "127.0.0.1", service.Addr().(*net.TCPAddr).IP.String())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.ListenAndServe(ctx) }()
	require.Eventually(t, func() bool {
		return service.local != nil
	}, time.Second, time.Millisecond)
	cancel()
	require.NoError(t, <-done)
	require.NoError(t, service.Close())

	reopened, err := agentstate.Open(home)
	require.NoError(t, err)
	require.NoError(t, reopened.Close())
}

func writeAgentConfig(t *testing.T, path, origin string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte("version: 1\nagent:\n  trusted_origins:\n    - "+origin+"\n"), 0o600))
}

func executableFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pi")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700))
	return path
}
