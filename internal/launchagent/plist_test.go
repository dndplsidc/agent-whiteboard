package launchagent

import (
	"bytes"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlistIsDeterministicAndStructurallySafe(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	executable := testRegularFile(t, filepath.Join(home, "bin & tools", "agent-whiteboard"), 0o700)
	configuration := testRegularFile(t, filepath.Join(home, "config & selected.yaml"), 0o600)
	provider := testRegularFile(t, filepath.Join(home, "providers", "pi & rpc"), 0o700)
	paths := pathsForHome(home)
	config := Config{
		Executable: executable,
		ConfigPath: configuration,
		ProviderExecutables: map[string]string{
			ProviderPi: provider,
		},
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
	assertStringValue(t, parsed, "Label", Label)
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
		PiExecutableEnvironment: normalized.ProviderExecutables[ProviderPi],
	})

	text := string(first)
	for _, forbidden := range []string{"UserName", "<key>PATH</key>", "sudo", "socket", "TOKEN", "PASSWORD", "SECRET", "credential"} {
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

	normalized, err := normalizeConfig(Config{
		Executable: executableLink,
		ConfigPath: configLink,
		ProviderExecutables: map[string]string{
			ProviderPi: providerLink,
		},
	})
	if err != nil {
		t.Fatalf("normalize symlinked paths: %v", err)
	}
	wantExecutable, _ := filepath.EvalSymlinks(realExecutable)
	wantConfig, _ := filepath.EvalSymlinks(realConfig)
	wantProvider, _ := filepath.EvalSymlinks(realProvider)
	if normalized.Executable != wantExecutable || normalized.ConfigPath != wantConfig || normalized.ProviderExecutables[ProviderPi] != wantProvider {
		t.Fatalf("paths were not resolved: %#v", normalized)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "relative executable", mutate: func(config *Config) { config.Executable = "agent-whiteboard" }},
		{name: "non executable", mutate: func(config *Config) { config.Executable = realConfig }},
		{name: "relative config", mutate: func(config *Config) { config.ConfigPath = "config.yaml" }},
		{name: "writable config", mutate: func(config *Config) {
			config.ConfigPath = testRegularFile(t, filepath.Join(home, "unsafe.yaml"), 0o666)
		}},
		{name: "unknown provider", mutate: func(config *Config) {
			config.ProviderExecutables = map[string]string{"unknown": realProvider}
		}},
		{name: "relative provider", mutate: func(config *Config) {
			config.ProviderExecutables = map[string]string{ProviderPi: "pi"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := Config{Executable: realExecutable, ConfigPath: realConfig}
			test.mutate(&config)
			if _, err := normalizeConfig(config); err == nil {
				t.Fatal("expected validation failure")
			}
		})
	}
}

func TestPlistOmitsEnvironmentWhenNoProviderOverrideIsExplicit(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	normalized, err := normalizeConfig(Config{
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
	if _, exists := parsed["EnvironmentVariables"]; exists {
		t.Fatal("unexpected inherited or empty environment dictionary")
	}
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
