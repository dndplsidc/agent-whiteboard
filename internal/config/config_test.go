package config_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/config"
	"github.com/stretchr/testify/require"
)

func TestLoadMissingDefaultReturnsEmptySparseConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	loaded, err := config.Load("")
	require.NoError(t, err)
	require.False(t, loaded.Exists())
	require.Equal(t, filepath.Join(home, ".agent-whiteboard", "config.yaml"), loaded.Path())
	require.Equal(t, 1, loaded.Version())
	_, set := loaded.Client().Server()
	require.False(t, set)
}

func TestLoadExplicitMissingPathErrors(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	defaultPath, err := config.DefaultPath()
	require.NoError(t, err)

	for _, path := range []string{filepath.Join(t.TempDir(), "missing.yaml"), defaultPath} {
		_, err := config.Load(path)
		require.ErrorContains(t, err, "read configuration")
	}
}

func TestLoadRejectsUnsafeConfigurationFiles(t *testing.T) {
	directory := t.TempDir()
	regular := filepath.Join(directory, "regular.yaml")
	writeConfig(t, regular, "version: 1\n")

	symlink := filepath.Join(directory, "symlink.yaml")
	require.NoError(t, os.Symlink(regular, symlink))
	_, err := config.Load(symlink)
	require.ErrorContains(t, err, "regular file")

	worldWritable := filepath.Join(directory, "world-writable.yaml")
	writeConfig(t, worldWritable, "version: 1\n")
	require.NoError(t, os.Chmod(worldWritable, 0o666))
	_, err = config.Load(worldWritable)
	require.ErrorContains(t, err, "must not be writable by group or others")

	sharedReadOnly := filepath.Join(directory, "shared-read-only.yaml")
	writeConfig(t, sharedReadOnly, "version: 1\n")
	require.NoError(t, os.Chmod(sharedReadOnly, 0o644))
	_, err = config.Load(sharedReadOnly)
	require.NoError(t, err)
}

func TestLoadMinimalVersionedConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, "version: 1\n")

	loaded, err := config.Load(path)
	require.NoError(t, err)
	require.True(t, loaded.Exists())
	_, set := loaded.Agent().Port()
	require.False(t, set)
}

func TestLoadSparseConfiguration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeConfig(t, path, `version: 1
client:
  server: https://whiteboard.example
  timeout: 45s
server:
  host: 0.0.0.0
  port: 9000
  storage: data
  cleanup_interval: 5m
  default_expires_in: 0
  shutdown_timeout: 8s
  log_mode: json
  max_whiteboard_bytes: 100
  max_context_bytes: 101
  max_image_bytes: 102
  max_image_request_bytes: 103
viewer:
  local_agent:
    enabled: true
agent:
  port: 9444
  trusted_origins:
    - https://WHITEBOARD.example:443
    - https://other.example:8443
  provider_idle_timeout: 2h
  shutdown_timeout: 12s
  default_access: content-only
`)

	loaded, err := config.Load(path)
	require.NoError(t, err)
	require.True(t, loaded.Exists())
	require.Equal(t, path, loaded.Path())

	server, set := loaded.Client().Server()
	require.True(t, set)
	require.Equal(t, "https://whiteboard.example", server)
	timeout, set := loaded.Client().Timeout()
	require.True(t, set)
	require.Equal(t, 45*time.Second, timeout)

	host, set := loaded.Server().Host()
	require.True(t, set)
	require.Equal(t, "0.0.0.0", host)
	port, set := loaded.Server().Port()
	require.True(t, set)
	require.Equal(t, 9000, port)
	storage, set := loaded.Server().Storage()
	require.True(t, set)
	require.Equal(t, filepath.Join(dir, "data"), storage)
	cleanup, set := loaded.Server().CleanupInterval()
	require.True(t, set)
	require.Equal(t, 5*time.Minute, cleanup)
	expires, set := loaded.Server().DefaultExpiresIn()
	require.True(t, set)
	require.Zero(t, expires)
	shutdown, set := loaded.Server().ShutdownTimeout()
	require.True(t, set)
	require.Equal(t, 8*time.Second, shutdown)
	logMode, set := loaded.Server().LogMode()
	require.True(t, set)
	require.Equal(t, "json", logMode)
	whiteboardLimit, set := loaded.Server().MaxWhiteboardBytes()
	require.True(t, set)
	require.EqualValues(t, 100, whiteboardLimit)
	contextLimit, set := loaded.Server().MaxContextBytes()
	require.True(t, set)
	require.EqualValues(t, 101, contextLimit)
	imageLimit, set := loaded.Server().MaxImageBytes()
	require.True(t, set)
	require.EqualValues(t, 102, imageLimit)
	requestLimit, set := loaded.Server().MaxImageRequestBytes()
	require.True(t, set)
	require.EqualValues(t, 103, requestLimit)

	enabled, set := loaded.Viewer().LocalAgent().Enabled()
	require.True(t, set)
	require.True(t, enabled)
	agentPort, set := loaded.Agent().Port()
	require.True(t, set)
	require.Equal(t, 9444, agentPort)
	origins, set := loaded.Agent().TrustedOrigins()
	require.True(t, set)
	require.Equal(t, []string{"https://whiteboard.example", "https://other.example:8443"}, origins)
	origins[0] = "mutated"
	originsAgain, _ := loaded.Agent().TrustedOrigins()
	require.Equal(t, "https://whiteboard.example", originsAgain[0])
	idle, set := loaded.Agent().ProviderIdleTimeout()
	require.True(t, set)
	require.Equal(t, 2*time.Hour, idle)
	agentShutdown, set := loaded.Agent().ShutdownTimeout()
	require.True(t, set)
	require.Equal(t, 12*time.Second, agentShutdown)
	access, set := loaded.Agent().DefaultAccess()
	require.True(t, set)
	require.Equal(t, config.AccessContentOnly, access)
}

func TestBuiltins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	defaults, err := config.Builtins()
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:8567", defaults.Client.Server)
	require.Equal(t, 30*time.Second, defaults.Client.Timeout)
	require.Equal(t, "127.0.0.1", defaults.Server.Host)
	require.Equal(t, 8567, defaults.Server.Port)
	require.Equal(t, filepath.Join(home, ".agent-whiteboard"), defaults.Server.Storage)
	require.Equal(t, 15*time.Minute, defaults.Server.CleanupInterval)
	require.EqualValues(t, 86400, defaults.Server.DefaultExpiresIn)
	require.Equal(t, 10*time.Second, defaults.Server.ShutdownTimeout)
	require.Equal(t, "console", defaults.Server.LogMode)
	require.EqualValues(t, 10<<20, defaults.Server.MaxWhiteboardBytes)
	require.EqualValues(t, 1<<20, defaults.Server.MaxContextBytes)
	require.EqualValues(t, 25<<20, defaults.Server.MaxImageBytes)
	require.EqualValues(t, 100<<20, defaults.Server.MaxImageRequestBytes)
	require.False(t, defaults.Viewer.LocalAgent.Enabled)
	require.Equal(t, 8568, defaults.Agent.Port)
	require.Empty(t, defaults.Agent.TrustedOrigins)
	require.Equal(t, 60*time.Minute, defaults.Agent.ProviderIdleTimeout)
	require.Equal(t, 10*time.Second, defaults.Agent.ShutdownTimeout)
	require.Equal(t, config.AccessContentOnly, defaults.Agent.DefaultAccess)
}

func TestResolvePath(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("HOME", home)
	oldCWD, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(cwd))
	t.Cleanup(func() { require.NoError(t, os.Chdir(oldCWD)) })
	cwd, err = os.Getwd()
	require.NoError(t, err)

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "relative", path: "settings/config.yaml", want: filepath.Join(cwd, "settings", "config.yaml")},
		{name: "home", path: "~", want: home},
		{name: "home child", path: "~/.agent-whiteboard/config.yaml", want: filepath.Join(home, ".agent-whiteboard", "config.yaml")},
		{name: "absolute lexical", path: filepath.Join(cwd, "link", "..", "config.yaml"), want: filepath.Join(cwd, "config.yaml")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := config.ResolvePath(test.path)
			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}

	_, err = config.ResolvePath("~another/config.yaml")
	require.ErrorContains(t, err, "named-user home expansion")
	_, err = config.ResolvePath("")
	require.ErrorContains(t, err, "path is required")
}

func TestStoragePathUsesLexicalConfigDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behavior differs on Windows")
	}
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	linkDir := filepath.Join(root, "link")
	require.NoError(t, os.Mkdir(realDir, 0o700))
	require.NoError(t, os.Symlink(realDir, linkDir))
	path := filepath.Join(linkDir, "config.yaml")
	writeConfig(t, path, "version: 1\nserver:\n  storage: data\n")

	loaded, err := config.Load(path)
	require.NoError(t, err)
	storage, set := loaded.Server().Storage()
	require.True(t, set)
	require.Equal(t, filepath.Join(linkDir, "data"), storage)
}

func TestLoadRejectsInvalidYAMLContracts(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{name: "empty", yaml: ""},
		{name: "sequence root", yaml: "- version: 1\n"},
		{name: "scalar root", yaml: "1\n"},
		{name: "missing version", yaml: "client: {}\n"},
		{name: "unsupported version", yaml: "version: 2\n"},
		{name: "version string", yaml: "version: '1'\n"},
		{name: "unknown root field", yaml: "version: 1\nbogus: true\n"},
		{name: "unknown nested field", yaml: "version: 1\nviewer:\n  local_agent:\n    bogus: true\n"},
		{name: "unknown empty section child", yaml: "version: 1\nclient:\n  bogus: value\n"},
		{name: "duplicate root key", yaml: "version: 1\nversion: 1\n"},
		{name: "duplicate nested key", yaml: "version: 1\nagent:\n  port: 8568\n  port: 9000\n"},
		{name: "alias scalar", yaml: "version: 1\nclient:\n  server: &server https://example.test\n  timeout: *server\n"},
		{name: "alias mapping", yaml: "version: 1\nclient: &client\n  timeout: 1s\nserver: *client\n"},
		{name: "merge", yaml: "version: 1\nclient: &client\n  timeout: 1s\nserver:\n  <<: *client\n"},
		{name: "multiple documents", yaml: "version: 1\n---\nversion: 1\n"},
		{name: "trailing empty document", yaml: "version: 1\n---\n"},
		{name: "invalid bool type", yaml: "version: 1\nviewer:\n  local_agent:\n    enabled: yes\n"},
		{name: "invalid integer type", yaml: "version: 1\nagent:\n  port: '8568'\n"},
		{name: "invalid duration type", yaml: "version: 1\nagent:\n  shutdown_timeout: 10\n"},
		{name: "invalid sequence type", yaml: "version: 1\nagent:\n  trusted_origins: https://example.test\n"},
		{name: "null section", yaml: "version: 1\nagent: null\n"},
		{name: "broker host prohibited", yaml: "version: 1\nagent:\n  host: 0.0.0.0\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			writeConfig(t, path, test.yaml)
			_, err := config.Load(path)
			require.Error(t, err)
		})
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{name: "client duration zero", yaml: "version: 1\nclient:\n  timeout: 0s\n"},
		{name: "server duration negative", yaml: "version: 1\nserver:\n  cleanup_interval: -1s\n"},
		{name: "server port negative", yaml: "version: 1\nserver:\n  port: -1\n"},
		{name: "server port too large", yaml: "version: 1\nserver:\n  port: 65536\n"},
		{name: "agent port zero", yaml: "version: 1\nagent:\n  port: 0\n"},
		{name: "agent port too large", yaml: "version: 1\nagent:\n  port: 65536\n"},
		{name: "negative expiration", yaml: "version: 1\nserver:\n  default_expires_in: -1\n"},
		{name: "negative limit", yaml: "version: 1\nserver:\n  max_context_bytes: -1\n"},
		{name: "request below image", yaml: "version: 1\nserver:\n  max_image_bytes: 20\n  max_image_request_bytes: 10\n"},
		{name: "bad log mode", yaml: "version: 1\nserver:\n  log_mode: text\n"},
		{name: "bad access", yaml: "version: 1\nagent:\n  default_access: full\n"},
		{name: "empty storage", yaml: "version: 1\nserver:\n  storage: ''\n"},
		{name: "named-user storage", yaml: "version: 1\nserver:\n  storage: ~someone/data\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			writeConfig(t, path, test.yaml)
			_, err := config.Load(path)
			require.Error(t, err)
		})
	}
}

func writeConfig(t *testing.T, path, contents string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
}
