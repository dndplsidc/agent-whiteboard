package config_test

import (
	"path/filepath"
	"testing"

	"github.com/dndplsidc/agent-whiteboard/internal/config"
	"github.com/stretchr/testify/require"
)

func TestCanonicalOrigin(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "https://WHITEBOARD.EXAMPLE", want: "https://whiteboard.example"},
		{input: "HTTPS://Whiteboard.Example:443", want: "https://whiteboard.example"},
		{input: "https://whiteboard.example:0443", want: "https://whiteboard.example"},
		{input: "https://whiteboard.example:8443", want: "https://whiteboard.example:8443"},
		{input: "https://[2001:DB8::1]:443", want: "https://[2001:db8::1]"},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, err := config.CanonicalOrigin(test.input)
			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
}

func TestCanonicalBrowserOriginAllowsHTTPSAndLiteralLoopbackHTTP(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "https://WHITEBOARD.EXAMPLE:443", want: "https://whiteboard.example"},
		{input: "http://127.0.0.1", want: "http://127.0.0.1"},
		{input: "HTTP://127.0.0.1:8080", want: "http://127.0.0.1:8080"},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, err := config.CanonicalBrowserOrigin(test.input)
			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
}

func TestCanonicalBrowserOriginRejectsLoopbackHTTPNearMatches(t *testing.T) {
	inputs := []string{
		"http://localhost:8080",
		"http://127.0.0.2:8080",
		"http://127.1:8080",
		"http://0177.0.0.1:8080",
		"http://[::1]:8080",
		"http://user@127.0.0.1:8080",
		"http://127.0.0.1:0",
		"http://127.0.0.1:65536",
		"http://127.0.0.1:8080/",
		"http://127.0.0.1:8080?query=yes",
		"http://127.0.0.1:8080#fragment",
		"http://example.test",
	}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			_, err := config.CanonicalBrowserOrigin(input)
			require.Error(t, err)
		})
	}
}

func TestCanonicalOriginRejectsNonExactHTTPSOrigins(t *testing.T) {
	inputs := []string{
		"",
		"http://whiteboard.example",
		"http://127.0.0.1:8080",
		"https://*.example",
		"https://user@whiteboard.example",
		"https://whiteboard.example/",
		"https://whiteboard.example/path",
		"https://whiteboard.example?query=yes",
		"https://whiteboard.example#fragment",
		"https://whiteboard.example:0",
		"https://whiteboard.example:65536",
		"https://whiteboard.example:not-a-port",
		"https://whiteboard.example:8443:9443",
		"https://whiteboard.example%2eevil.test",
		"https://[fe80::1%25lo0]",
		"https://[example.com]",
		"https://[127.0.0.1]",
		" https://whiteboard.example",
		"https://whiteboard.example ",
		"null",
	}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			_, err := config.CanonicalOrigin(input)
			require.Error(t, err)
		})
	}
}

func TestLoadRejectsOriginsThatDuplicateAfterCanonicalization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, `version: 1
agent:
  trusted_origins:
    - https://WHITEBOARD.example:443
    - https://whiteboard.example
`)

	_, err := config.Load(path)
	require.ErrorContains(t, err, "duplicate trusted origin")
}
