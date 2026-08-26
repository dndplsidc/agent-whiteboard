package common

import (
	"bytes"
	"encoding/xml"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestExactContractLiterals(t *testing.T) {
	if LaunchAgentPiExecutableEnvironment != "AGENT_WHITEBOARD_PROVIDER_PI_EXECUTABLE" {
		t.Fatalf("Pi environment key = %q", LaunchAgentPiExecutableEnvironment)
	}
	if LaunchAgentCodexExecutableEnvironment != "AGENT_WHITEBOARD_PROVIDER_CODEX_EXECUTABLE" {
		t.Fatalf("Codex environment key = %q", LaunchAgentCodexExecutableEnvironment)
	}
	if LaunchAgentPathEnvironment != "PATH" {
		t.Fatalf("PATH environment key = %q", LaunchAgentPathEnvironment)
	}
	if got := unsupportedGuidance("linux"); got != "managed agent daemon is unsupported on linux; run 'agent-whiteboard agent serve' in the foreground" {
		t.Fatalf("Linux guidance = %q", got)
	}
	if got := unsupportedGuidance("windows"); strings.Contains(got, "`") || !strings.Contains(got, "unsupported on windows") {
		t.Fatalf("other-GOOS guidance is not actionable: %q", got)
	}
}

func TestPlistIsDeterministicAndStructurallySafe(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	executable := testRegularFile(t, filepath.Join(home, "bin & tools", "agent-whiteboard"), 0o700)
	configuration := testRegularFile(t, filepath.Join(home, "config & selected.yaml"), 0o600)
	piProvider := testRegularFile(t, filepath.Join(home, "providers", "pi & rpc"), 0o700)
	codexProvider := testRegularFile(t, filepath.Join(home, "providers", "codex & rpc"), 0o700)
	paths := pathsForHome(home)
	config := LaunchAgentConfig{
		Executable: executable,
		ConfigPath: configuration,
		Providers: []LaunchAgentProviderDescriptor{
			testProviderDescriptor{name: LaunchAgentProviderPi, executable: LaunchAgentProviderPi},
			testProviderDescriptor{name: LaunchAgentProviderCodex, executable: LaunchAgentProviderCodex},
		},
		ExecutableResolver: testExecutableResolver{paths: map[string]string{LaunchAgentProviderPi: piProvider, LaunchAgentProviderCodex: codexProvider}},
		EnvironmentPath:    filepath.Join(home, ".nvm", "versions", "node", "v22.0.0", "bin"),
	}

	normalized, err := normalizeConfig(config)
	if err != nil {
		t.Fatalf("normalize config: %v", err)
	}
	first, err := marshalPlist(normalized, paths)
	if err != nil {
		t.Fatalf("marshal plist: %v", err)
	}
	second, err := marshalPlist(normalized, paths)
	if err != nil {
		t.Fatalf("marshal plist again: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("plist output is not deterministic")
	}

	parsed := parsePlist(t, first)
	assertStringValue(t, parsed, "Label", LaunchAgentLabel)
	if _, exists := parsed["LaunchAgentLabel"]; exists {
		t.Fatal("plist contains the invalid LaunchAgentLabel key")
	}
	assertStringArray(t, parsed, "ProgramArguments", []string{
		normalized.Executable,
		"--config",
		normalized.ConfigPath,
		"agent",
		"serve",
	})
	assertBoolValue(t, parsed, "RunAtLoad", true)
	assertBoolValue(t, parsed, "KeepAlive", true)
	assertStringValue(t, parsed, "StandardOutPath", paths.StdoutLog)
	assertStringValue(t, parsed, "StandardErrorPath", paths.StderrLog)
	assertStringDictionary(t, parsed, "EnvironmentVariables", map[string]string{
		LaunchAgentPiExecutableEnvironment:    normalized.ProviderExecutables[LaunchAgentProviderPi],
		LaunchAgentCodexExecutableEnvironment: normalized.ProviderExecutables[LaunchAgentProviderCodex],
		LaunchAgentPathEnvironment: strings.Join([]string{
			filepath.Join(home, ".nvm", "versions", "node", "v22.0.0", "bin"),
			filepath.Join(home, ".bun", "bin"),
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, "bin"),
			filepath.Join(home, "go", "bin"),
			filepath.Join(home, ".asdf", "shims"),
			filepath.Join(home, ".local", "share", "mise", "shims"),
			filepath.Join(home, ".volta", "bin"),
			filepath.Join(home, ".nix-profile", "bin"),
			"/opt/homebrew/bin",
			"/opt/homebrew/sbin",
			"/usr/local/bin",
			"/nix/var/nix/profiles/default/bin",
			"/run/current-system/sw/bin",
			"/usr/bin",
			"/bin",
			"/usr/sbin",
			"/sbin",
		}, string(os.PathListSeparator)),
	})

	text := string(first)
	for _, forbidden := range []string{"UserName", "sudo", "socket", "TOKEN", "PASSWORD", "SECRET", "credential"} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(forbidden)) {
			t.Fatalf("plist contains forbidden value %q", forbidden)
		}
	}
	if !strings.Contains(text, "&amp;") {
		t.Fatal("plist paths were not XML-escaped")
	}
	orderedKeys := []string{"<key>Label</key>", "<key>ProgramArguments</key>", "<key>RunAtLoad</key>", "<key>KeepAlive</key>", "<key>StandardOutPath</key>", "<key>StandardErrorPath</key>", "<key>EnvironmentVariables</key>"}
	previous := -1
	for _, want := range orderedKeys {
		position := strings.Index(text, want)
		if position < 0 {
			t.Fatalf("plist does not contain %s", want)
		}
		if position <= previous {
			t.Fatalf("plist key %s is out of deterministic order", want)
		}
		previous = position
	}
}

func TestNormalizeConfigResolvesRealPathsAndRejectsUnsafeInputs(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	realExecutable := testRegularFile(t, filepath.Join(home, "real-agent-whiteboard"), 0o700)
	executableLink := filepath.Join(home, "agent-whiteboard")
	if err := os.Symlink(realExecutable, executableLink); err != nil {
		t.Fatal(err)
	}
	realConfig := testRegularFile(t, filepath.Join(home, "real-config.yaml"), 0o600)
	configLink := filepath.Join(home, "config.yaml")
	if err := os.Symlink(realConfig, configLink); err != nil {
		t.Fatal(err)
	}
	realProvider := testRegularFile(t, filepath.Join(home, "real-pi"), 0o700)
	providerLink := filepath.Join(home, "pi")
	if err := os.Symlink(realProvider, providerLink); err != nil {
		t.Fatal(err)
	}

	normalized, err := normalizeConfig(LaunchAgentConfig{
		Executable:         executableLink,
		ConfigPath:         configLink,
		Providers:          []LaunchAgentProviderDescriptor{testProviderDescriptor{name: "pi", executable: "pi"}},
		ExecutableResolver: testExecutableResolver{paths: map[string]string{"pi": providerLink}},
	})
	if err != nil {
		t.Fatalf("normalize symlinked paths: %v", err)
	}
	wantExecutable, _ := filepath.EvalSymlinks(realExecutable)
	wantConfig, _ := filepath.EvalSymlinks(realConfig)
	wantProvider, _ := filepath.EvalSymlinks(realProvider)
	if normalized.Executable != wantExecutable || normalized.ConfigPath != wantConfig || normalized.ProviderExecutables[LaunchAgentProviderPi] != wantProvider {
		t.Fatalf("paths were not resolved: %#v", normalized)
	}

	tests := []struct {
		name   string
		mutate func(*LaunchAgentConfig)
	}{
		{name: "relative executable", mutate: func(config *LaunchAgentConfig) { config.Executable = "agent-whiteboard" }},
		{name: "non executable", mutate: func(config *LaunchAgentConfig) { config.Executable = realConfig }},
		{name: "relative config", mutate: func(config *LaunchAgentConfig) { config.ConfigPath = "config.yaml" }},
		{name: "writable config", mutate: func(config *LaunchAgentConfig) {
			config.ConfigPath = testRegularFile(t, filepath.Join(home, "unsafe.yaml"), 0o666)
		}},
		{name: "unknown provider", mutate: func(config *LaunchAgentConfig) {
			config.Providers = []LaunchAgentProviderDescriptor{testProviderDescriptor{name: "unknown", executable: "unknown"}}
		}},
		{name: "unregistered executable name", mutate: func(config *LaunchAgentConfig) {
			config.Providers = []LaunchAgentProviderDescriptor{testProviderDescriptor{name: "pi", executable: "arbitrary"}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := LaunchAgentConfig{Executable: realExecutable, ConfigPath: realConfig}
			test.mutate(&config)
			if _, err := normalizeConfig(config); err == nil {
				t.Fatal("expected validation failure")
			}
		})
	}
}

func TestPlistAlwaysIncludesRuntimePathWithoutProviderOverrides(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	normalized, err := normalizeConfig(LaunchAgentConfig{
		Executable: testRegularFile(t, filepath.Join(home, "agent-whiteboard"), 0o700),
		ConfigPath: testRegularFile(t, filepath.Join(home, "config.yaml"), 0o600),
	})
	if err != nil {
		t.Fatal(err)
	}
	contents, err := marshalPlist(normalized, pathsForHome(home))
	if err != nil {
		t.Fatal(err)
	}
	parsed := parsePlist(t, contents)
	environment, exists := parsed["EnvironmentVariables"]
	if !exists {
		t.Fatal("runtime PATH environment dictionary is missing")
	}
	if len(environment.dictionary) != 1 || environment.dictionary[LaunchAgentPathEnvironment].text == "" {
		t.Fatalf("environment = %#v, want only a non-empty PATH", environment.dictionary)
	}
}

func TestLaunchAgentPathPreservesActivatedManagersAndAddsPortableDefaults(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	nvm := filepath.Join(home, ".nvm", "versions", "node", "v22.14.0", "bin")
	nixStore := "/nix/store/example-node/bin"
	ambient := strings.Join([]string{
		nvm,
		"relative/bin",
		"",
		nixStore,
		nvm,
		"/valid/../unclean",
		"/contains\ncontrol",
	}, string(os.PathListSeparator))

	got := launchAgentPathEntries(ambient, home)
	wantPrefix := []string{nvm, nixStore}
	if len(got) < len(wantPrefix) || !reflect.DeepEqual(got[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("PATH prefix = %#v, want activated manager paths %#v", got, wantPrefix)
	}
	for _, want := range []string{
		filepath.Join(home, ".bun", "bin"),
		filepath.Join(home, ".nix-profile", "bin"),
		"/opt/homebrew/bin",
		"/nix/var/nix/profiles/default/bin",
		"/run/current-system/sw/bin",
		"/usr/bin",
	} {
		if !containsString(got, want) {
			t.Fatalf("PATH does not contain default %q: %#v", want, got)
		}
	}
	for _, forbidden := range []string{"relative/bin", "", "/valid/../unclean", "/contains\ncontrol"} {
		if containsString(got, forbidden) {
			t.Fatalf("PATH retained unsafe entry %q: %#v", forbidden, got)
		}
	}
	if countString(got, nvm) != 1 {
		t.Fatalf("PATH did not deduplicate %q: %#v", nvm, got)
	}
}

func TestEnvShebangInterpreterIgnoresOrdinaryBinary(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "provider")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0}, 5000), 0o700); err != nil {
		t.Fatal(err)
	}
	interpreter, err := envShebangInterpreter(path)
	if err != nil {
		t.Fatalf("inspect binary provider: %v", err)
	}
	if interpreter != "" {
		t.Fatalf("binary provider interpreter = %q, want empty", interpreter)
	}
}

func TestPlistValidatesEnvShebangInterpreterAgainstRuntimePath(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	agent := testRegularFile(t, filepath.Join(home, "agent-whiteboard"), 0o700)
	configuration := testRegularFile(t, filepath.Join(home, "config.yaml"), 0o600)
	provider := testRegularFile(t, filepath.Join(home, "providers", "pi"), 0o700)
	if err := os.WriteFile(provider, []byte("#!/usr/bin/env managed-node\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := LaunchAgentConfig{
		Executable:         agent,
		ConfigPath:         configuration,
		Providers:          []LaunchAgentProviderDescriptor{testProviderDescriptor{name: "pi", executable: "pi"}},
		ExecutableResolver: testExecutableResolver{paths: map[string]string{"pi": provider}},
	}

	if _, err := GenerateLaunchAgentPlist(config, home); err == nil || !strings.Contains(err.Error(), `Pi provider interpreter "managed-node"`) {
		t.Fatalf("missing interpreter error = %v", err)
	}

	nvmBin := filepath.Join(home, ".nvm", "versions", "node", "v22.14.0", "bin")
	testRegularFile(t, filepath.Join(nvmBin, "managed-node"), 0o700)
	config.EnvironmentPath = nvmBin
	contents, err := GenerateLaunchAgentPlist(config, home)
	if err != nil {
		t.Fatalf("generate plist with activated NVM interpreter: %v", err)
	}
	environment := parsePlist(t, contents)["EnvironmentVariables"].dictionary
	if got := strings.Split(environment[LaunchAgentPathEnvironment].text, string(os.PathListSeparator))[0]; got != nvmBin {
		t.Fatalf("first PATH entry = %q, want %q", got, nvmBin)
	}
}

func containsString(values []string, want string) bool {
	return countString(values, want) > 0
}

func countString(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}

func TestNormalizeConfigRejectsTypedNilProviderDescriptor(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	executable := testRegularFile(t, filepath.Join(home, "agent-whiteboard"), 0o700)
	configPath := filepath.Join(home, "missing-config.yaml")
	var descriptor *nilTestProviderDescriptor
	if _, err := normalizeConfig(LaunchAgentConfig{
		Executable: executable,
		ConfigPath: configPath,
		Providers:  []LaunchAgentProviderDescriptor{descriptor},
	}); err == nil || !strings.Contains(err.Error(), "provider descriptor is required") {
		t.Fatalf("typed-nil descriptor error = %v", err)
	}
}

type nilTestProviderDescriptor struct{ name string }

func (descriptor *nilTestProviderDescriptor) ProviderName() string { return descriptor.name }
func (*nilTestProviderDescriptor) ExecutableName() string          { return "pi" }

func TestNormalizeConfigRejectsMissingConfigEscapingSymlinkedBoundary(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	outside := t.TempDir()
	executable := testRegularFile(t, filepath.Join(home, "agent-whiteboard"), 0o700)
	intended := filepath.Join(home, "intended")
	if err := os.Mkdir(intended, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(intended, "link")); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(intended, "link", "missing", "config.yaml")
	if _, err := normalizeConfig(LaunchAgentConfig{Executable: executable, ConfigPath: configPath}); err == nil {
		t.Fatal("missing configuration escaped through a symlinked ancestor")
	}
}

func TestNormalizeConfigAllowsAbsentDefaultConfiguration(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	executable := testRegularFile(t, filepath.Join(home, "bin", "agent-whiteboard"), 0o700)
	configPath := filepath.Join(home, ".agent-whiteboard", "config.yaml")
	normalized, err := normalizeConfig(LaunchAgentConfig{Executable: executable, ConfigPath: configPath})
	if err != nil {
		t.Fatalf("normalize absent default config: %v", err)
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	wantConfigPath := filepath.Join(realHome, ".agent-whiteboard", "config.yaml")
	if normalized.ConfigPath != wantConfigPath {
		t.Fatalf("ConfigPath = %q, want absent default %q", normalized.ConfigPath, wantConfigPath)
	}
	contents, err := marshalPlist(normalized, pathsForHome(home))
	if err != nil {
		t.Fatal(err)
	}
	assertStringArray(t, parsePlist(t, contents), "ProgramArguments", []string{
		normalized.Executable, "--config", wantConfigPath, "agent", "serve",
	})
}

func TestProviderResolverFoundMissingAndUntrustedResults(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	base := LaunchAgentConfig{
		Executable: testRegularFile(t, filepath.Join(home, "agent-whiteboard"), 0o700),
		ConfigPath: filepath.Join(home, "missing-config.yaml"),
		Providers:  []LaunchAgentProviderDescriptor{testProviderDescriptor{name: "pi", executable: "pi"}},
	}
	provider := testRegularFile(t, filepath.Join(home, "bin", "pi"), 0o700)
	found := base
	found.ExecutableResolver = testExecutableResolver{paths: map[string]string{"pi": provider}}
	normalized, err := normalizeConfig(found)
	if err != nil {
		t.Fatalf("resolve found provider: %v", err)
	}
	resolvedProvider, _ := filepath.EvalSymlinks(provider)
	if normalized.ProviderExecutables["pi"] != resolvedProvider {
		t.Fatalf("provider path = %q, want %q", normalized.ProviderExecutables["pi"], resolvedProvider)
	}

	missing := base
	missing.ExecutableResolver = testExecutableResolver{}
	normalized, err = normalizeConfig(missing)
	if err != nil {
		t.Fatalf("missing provider must be nonfatal: %v", err)
	}
	if len(normalized.ProviderExecutables) != 0 {
		t.Fatalf("missing provider recorded: %#v", normalized.ProviderExecutables)
	}

	untrusted := testRegularFile(t, filepath.Join(home, "bin", "not-current-user-executable"), 0o001)
	unsafe := base
	unsafe.ExecutableResolver = testExecutableResolver{paths: map[string]string{"pi": untrusted}}
	if _, err := normalizeConfig(unsafe); err == nil {
		t.Fatal("resolver result inaccessible to the current user was accepted")
	}

	resolverFailure := base
	resolverFailure.ExecutableResolver = testExecutableResolver{err: errors.New("resolver failed")}
	if _, err := normalizeConfig(resolverFailure); err == nil || !strings.Contains(err.Error(), "resolver failed") {
		t.Fatalf("resolver error = %v", err)
	}
}

func TestProviderResolverResolvesPiAndCodexIndependently(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	piProvider := testRegularFile(t, filepath.Join(home, "bin", LaunchAgentProviderPi), 0o700)
	base := LaunchAgentConfig{
		Executable: testRegularFile(t, filepath.Join(home, "agent-whiteboard"), 0o700),
		ConfigPath: filepath.Join(home, "missing-config.yaml"),
		Providers: []LaunchAgentProviderDescriptor{
			testProviderDescriptor{name: LaunchAgentProviderPi, executable: LaunchAgentProviderPi},
			testProviderDescriptor{name: LaunchAgentProviderCodex, executable: LaunchAgentProviderCodex},
		},
		ExecutableResolver: testExecutableResolver{paths: map[string]string{LaunchAgentProviderPi: piProvider}},
	}

	normalized, err := normalizeConfig(base)
	if err != nil {
		t.Fatalf("normalize with missing default Codex: %v", err)
	}
	if _, exists := normalized.ProviderExecutables[LaunchAgentProviderCodex]; exists {
		t.Fatalf("missing Codex executable recorded: %#v", normalized.ProviderExecutables)
	}
	resolvedPi, _ := filepath.EvalSymlinks(piProvider)
	if normalized.ProviderExecutables[LaunchAgentProviderPi] != resolvedPi {
		t.Fatalf("Pi executable = %q, want %q", normalized.ProviderExecutables[LaunchAgentProviderPi], resolvedPi)
	}
}

type testProviderDescriptor struct {
	name       string
	executable string
}

func (descriptor testProviderDescriptor) ProviderName() string   { return descriptor.name }
func (descriptor testProviderDescriptor) ExecutableName() string { return descriptor.executable }

type testExecutableResolver struct {
	paths map[string]string
	err   error
}

func (resolver testExecutableResolver) LookPath(name string) (string, error) {
	if resolver.err != nil {
		return "", resolver.err
	}
	if path, ok := resolver.paths[name]; ok {
		return path, nil
	}
	return "", exec.ErrNotFound
}

func testRegularFile(t *testing.T, path string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("test"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

type plistValue struct {
	kind       string
	text       string
	boolean    bool
	array      []plistValue
	dictionary map[string]plistValue
}

func parsePlist(t *testing.T, contents []byte) map[string]plistValue {
	t.Helper()
	decoder := xml.NewDecoder(bytes.NewReader(contents))
	for {
		token, err := decoder.Token()
		if err != nil {
			t.Fatalf("parse plist XML: %v", err)
		}
		start, ok := token.(xml.StartElement)
		if ok && start.Name.Local == "dict" {
			value := parseDictionary(t, decoder)
			return value.dictionary
		}
	}
}

func parseDictionary(t *testing.T, decoder *xml.Decoder) plistValue {
	t.Helper()
	values := make(map[string]plistValue)
	for {
		token, err := decoder.Token()
		if err != nil {
			t.Fatalf("parse dictionary: %v", err)
		}
		switch typed := token.(type) {
		case xml.EndElement:
			if typed.Name.Local == "dict" {
				return plistValue{kind: "dict", dictionary: values}
			}
		case xml.StartElement:
			if typed.Name.Local != "key" {
				t.Fatalf("expected dictionary key, got %s", typed.Name.Local)
			}
			var key string
			if err := decoder.DecodeElement(&key, &typed); err != nil {
				t.Fatal(err)
			}
			values[key] = parseValue(t, decoder)
		}
	}
}

func parseValue(t *testing.T, decoder *xml.Decoder) plistValue {
	t.Helper()
	for {
		token, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "string", "integer":
			var text string
			if err := decoder.DecodeElement(&text, &start); err != nil {
				t.Fatal(err)
			}
			return plistValue{kind: start.Name.Local, text: text}
		case "true", "false":
			if err := decoder.Skip(); err != nil {
				t.Fatal(err)
			}
			return plistValue{kind: "bool", boolean: start.Name.Local == "true"}
		case "dict":
			return parseDictionary(t, decoder)
		case "array":
			var values []plistValue
			for {
				token, err := decoder.Token()
				if err != nil {
					t.Fatal(err)
				}
				if end, ok := token.(xml.EndElement); ok && end.Name.Local == "array" {
					return plistValue{kind: "array", array: values}
				}
				if child, ok := token.(xml.StartElement); ok {
					if child.Name.Local != "string" {
						t.Fatalf("unexpected array value %s", child.Name.Local)
					}
					var text string
					if err := decoder.DecodeElement(&text, &child); err != nil {
						t.Fatal(err)
					}
					values = append(values, plistValue{kind: "string", text: text})
				}
			}
		default:
			t.Fatalf("unexpected plist value %s", start.Name.Local)
		}
	}
}

func assertStringValue(t *testing.T, values map[string]plistValue, key, want string) {
	t.Helper()
	if got := values[key]; got.kind != "string" || got.text != want {
		t.Fatalf("%s = %#v, want string %q", key, got, want)
	}
}

func assertBoolValue(t *testing.T, values map[string]plistValue, key string, want bool) {
	t.Helper()
	if got := values[key]; got.kind != "bool" || got.boolean != want {
		t.Fatalf("%s = %#v, want %v", key, got, want)
	}
}

func assertStringArray(t *testing.T, values map[string]plistValue, key string, want []string) {
	t.Helper()
	got := values[key]
	if got.kind != "array" || len(got.array) != len(want) {
		t.Fatalf("%s = %#v, want %#v", key, got, want)
	}
	for index := range want {
		if got.array[index].kind != "string" || got.array[index].text != want[index] {
			t.Fatalf("%s[%d] = %#v, want %q", key, index, got.array[index], want[index])
		}
	}
}

func assertStringDictionary(t *testing.T, values map[string]plistValue, key string, want map[string]string) {
	t.Helper()
	got := values[key]
	if got.kind != "dict" || len(got.dictionary) != len(want) {
		t.Fatalf("%s = %#v, want %#v", key, got, want)
	}
	for dictionaryKey, value := range want {
		assertStringValue(t, got.dictionary, dictionaryKey, value)
	}
}
