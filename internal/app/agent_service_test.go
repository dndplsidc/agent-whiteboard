package app

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/agent/broker"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/protocol"
	"github.com/dndplsidc/agent-whiteboard/internal/agent/provider"
	statepkg "github.com/dndplsidc/agent-whiteboard/internal/agent/state"
	"github.com/dndplsidc/agent-whiteboard/internal/common"
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

func TestProviderEnvironmentInheritsAmbientAndOverrideIsCloned(t *testing.T) {
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
	require.Contains(t, environment, "HOME=/host/home")
	require.Contains(t, environment, "USERPROFILE=/host/profile")
	require.Contains(t, environment, "PATH=/safe/bin")
	require.Contains(t, environment, "OPENAI_API_KEY=host-secret")
	require.Contains(t, environment, "HTTP_PROXY=http://user:password@proxy.invalid")
	require.Contains(t, environment, "AGENT_WHITEBOARD_AGENT_PORT=host-control")

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

func TestDefaultCodexEnvironmentInheritsAmbientUnchanged(t *testing.T) {
	originalCodexHome, hadCodexHome := os.LookupEnv("CODEX_HOME")
	require.NoError(t, os.Unsetenv("CODEX_HOME"))
	t.Cleanup(func() {
		if hadCodexHome {
			require.NoError(t, os.Setenv("CODEX_HOME", originalCodexHome))
			return
		}
		require.NoError(t, os.Unsetenv("CODEX_HOME"))
	})
	t.Setenv("HOME", "/ambient/codex-home")

	want := os.Environ()
	got, err := defaultCodexEnvironment(nil)
	require.NoError(t, err)
	require.Len(t, got, len(want))
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Codex environment differs from ambient environment at index %d", i)
		}
	}
	require.Contains(t, got, "HOME=/ambient/codex-home")
	for _, entry := range got {
		require.False(t, strings.HasPrefix(entry, "CODEX_HOME="))
	}
}

func TestDefaultCodexEnvironmentClonesAndValidatesOverride(t *testing.T) {
	override := []string{"HOME=/explicit/home", "CODEX_HOME=/explicit/codex", "TOKEN=kept"}
	got, err := defaultCodexEnvironment(override)
	require.NoError(t, err)
	override[0] = "HOME=mutated"
	require.Equal(t, []string{"HOME=/explicit/home", "CODEX_HOME=/explicit/codex", "TOKEN=kept"}, got)

	empty, err := defaultCodexEnvironment([]string{})
	require.NoError(t, err)
	require.NotNil(t, empty)

	_, err = defaultCodexEnvironment([]string{"A=1", "A=2"})
	require.ErrorContains(t, err, "duplicate")
	_, err = defaultCodexEnvironment([]string{"A=bad\x00value"})
	require.ErrorContains(t, err, "NUL")
}

func TestResolveCodexExecutableOptionalDefaultAndExplicitErrors(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	resolved, available, err := resolveCodexExecutable("")
	require.NoError(t, err)
	require.Empty(t, resolved)
	require.False(t, available)

	_, _, err = resolveCodexExecutable("missing-explicit-codex")
	require.ErrorContains(t, err, "resolve Codex executable")
	_, _, err = resolveCodexExecutable(filepath.Join(t.TempDir(), "missing-codex"))
	require.ErrorContains(t, err, "resolve Codex executable")
}

func TestResolveCodexExecutableFindsExplicitNamedExecutable(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "named-codex")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700))
	t.Setenv("PATH", directory)

	resolved, available, err := resolveCodexExecutable("named-codex")
	require.NoError(t, err)
	require.True(t, available)
	require.Equal(t, executable, resolved)
}

func TestResolvePiExecutableOptionalDefaultAndExplicitErrors(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	resolved, available, err := resolvePiExecutable("")
	require.NoError(t, err)
	require.Empty(t, resolved)
	require.False(t, available)

	_, _, err = resolvePiExecutable("missing-explicit-pi")
	require.ErrorContains(t, err, "resolve Pi executable")
	_, _, err = resolvePiExecutable(filepath.Join(t.TempDir(), "missing-pi"))
	require.ErrorContains(t, err, "resolve Pi executable")
}

func TestResolvePiExecutableFindsExplicitNamedExecutable(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "named-pi")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700))
	t.Setenv("PATH", directory)

	resolved, available, err := resolvePiExecutable("named-pi")
	require.NoError(t, err)
	require.True(t, available)
	require.Equal(t, executable, resolved)
}

func TestProviderEnvironmentCompositionRegistersAvailableProviders(t *testing.T) {
	for _, test := range []struct {
		name           string
		piAvailable    bool
		codexAvailable bool
		missing        []provider.Name
		wantPiRoot     bool
		wantCodexRoot  bool
	}{
		{name: "Pi and Codex", piAvailable: true, codexAvailable: true, wantPiRoot: true, wantCodexRoot: true},
		{name: "Pi only", piAvailable: true, missing: []provider.Name{provider.NameCodex}, wantPiRoot: true},
		{name: "Codex only", codexAvailable: true, missing: []provider.Name{provider.NamePi}, wantCodexRoot: true},
		{name: "neither provider", missing: []provider.Name{provider.NamePi, provider.NameCodex}},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			state, err := statepkg.Open(home)
			require.NoError(t, err)
			defer func() { require.NoError(t, state.Close()) }()

			executable := executableFixture(t)
			registry, err := newProviderRegistry(providerRegistryConfig{
				state: state, piExecutable: executable, piEnvironment: []string{}, piAvailable: test.piAvailable,
				codexExecutable: executable, codexEnvironment: []string{}, codexAvailable: test.codexAvailable,
				launcher: common.NewProcessGroupLauncher(), ids: common.CryptoIDGenerator{}, clock: common.SystemClock{},
				idleTimeout: time.Second,
			})
			require.NoError(t, err)
			require.Equal(t, provider.AllNames(), registry.Names())

			backend, err := broker.New(broker.Config{
				State: state, Drivers: registry, IDs: common.CryptoIDGenerator{}, Clock: common.SystemClock{}, Timers: broker.RealTimerFactory{},
				IdleTimeout: time.Second, ShutdownTimeout: time.Second,
			})
			require.NoError(t, err)
			for _, name := range test.missing {
				require.Equal(t, provider.Readiness{State: provider.MissingExecutable, Provider: name}, registry.Lookup(name).Readiness(context.Background()))
				_, connectErr := backend.Connect(context.Background(), "https://page.example", unavailableProviderConnect(name))
				var brokerErr broker.BrokerError
				require.ErrorAs(t, connectErr, &brokerErr)
				require.Equal(t, protocol.ErrorProviderMissing, brokerErr.Code())
			}
			require.NoError(t, backend.Close(context.Background()))

			piRoot := filepath.Join(home, ".agent-whiteboard", "state", "providers", string(provider.NamePi))
			_, err = os.Stat(piRoot)
			if test.wantPiRoot {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, os.ErrNotExist)
			}
			codexRoot := filepath.Join(home, ".agent-whiteboard", "state", "providers", string(provider.NameCodex))
			_, err = os.Stat(codexRoot)
			if test.wantCodexRoot {
				require.NoError(t, err)
				require.NotEqual(t, piRoot, codexRoot)
			} else {
				require.ErrorIs(t, err, os.ErrNotExist)
			}
		})
	}
}

func unavailableProviderConnect(name provider.Name) protocol.Command {
	created := time.Date(2026, 8, 5, 1, 2, 3, 0, time.UTC)
	expires := created.Add(time.Hour)
	return protocol.Command{
		APIVersion: protocol.APIVersion,
		CommandID:  strings.Repeat("a", 32),
		ClientID:   strings.Repeat("b", 32),
		Type:       protocol.CommandConnect,
		Payload: protocol.ConnectPayload{
			Provider:      protocol.ProviderName(name),
			Resource:      protocol.Resource{Kind: protocol.ResourceMarkdown, ID: strings.Repeat("c", 32), CreatedAt: created, UpdatedAt: created, ExpiresAt: &expires},
			ContextDigest: strings.Repeat("0", 64),
		},
	}
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

	reopened, err := statepkg.Open(home)
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
	require.NotNil(t, service.attachments)
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

	reopened, err := statepkg.Open(home)
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
