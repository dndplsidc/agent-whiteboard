package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDocumentationContracts(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	readme := readDocumentation(t, filepath.Join(root, "README.md"))
	httpAPI := readDocumentation(t, filepath.Join(root, "docs", "http-api.md"))

	commands := documentedCLICommands(t, readme)
	expectedCommands := []string{
		"serve", "create markdown", "create html", "update markdown", "update html",
		"delete markdown", "delete html", "image upload", "image update", "image delete",
	}
	for _, command := range expectedCommands {
		require.Contains(t, commands, command, "README must demonstrate %q", command)
	}
	for _, command := range commands {
		requireCommandHelp(t, strings.Fields(command))
	}

	routes := []string{
		"GET /healthz", "GET /readyz",
		"POST /api/v1/whiteboards/markdown", "PUT /api/v1/whiteboards/markdown/{id}", "DELETE /api/v1/whiteboards/markdown/{id}",
		"POST /api/v1/whiteboards/html", "GET /api/v1/whiteboards/html/{id}", "PUT /api/v1/whiteboards/html/{id}", "DELETE /api/v1/whiteboards/html/{id}",
		"GET /whiteboards/markdown/{id}", "GET /whiteboards/html/{id}", "GET /whiteboards/html/{id}/content",
		"POST /api/v1/images", "PUT /api/v1/images/{id}", "DELETE /api/v1/images/{id}", "GET /images/{id}",
	}
	for _, route := range routes {
		require.Contains(t, httpAPI, route, "HTTP API documentation is missing %s", route)
	}

	documentPaths := []string{"docs/http-api.md", "docs/go-api.md", "docs/storage.md", "docs/security.md", "docs/cli-json.md"}
	for _, document := range documentPaths {
		require.Contains(t, readme, document)
		_ = readDocumentation(t, filepath.Join(root, filepath.FromSlash(document)))
	}

	examplePaths := []string{"docs/examples/diagram.md", "docs/examples/standalone.html"}
	for _, example := range examplePaths {
		require.Contains(t, readme, example)
	}
	server := startServer(t)
	markdownPath := filepath.Join(root, examplePaths[0])
	htmlPath := filepath.Join(root, examplePaths[1])
	creatorContext := writeContextFixture(t, "# Example context\n\nThe diagram documentation fixture is published for validation.\n")
	markdown := runCLIResource(t, server, "--json", "create", "markdown", "--context", creatorContext, "--expires-in", "0", markdownPath)
	html := runCLIResource(t, server, "--json", "create", "html", "--context", creatorContext, "--expires-in", "0", htmlPath)
	markdownResponse, _ := fetch(t, markdown.Resource.URL)
	require.Equal(t, 200, markdownResponse.StatusCode)
	htmlResponse, htmlBody := fetch(t, html.Resource.URL)
	require.Equal(t, 200, htmlResponse.StatusCode)
	require.NotContains(t, htmlBody, "Agent Whiteboard Example")
	require.Contains(t, htmlBody, `src="/whiteboards/html/`+html.Resource.ID+`/content" sandbox="allow-scripts"`)
	wantHTML, err := os.ReadFile(htmlPath)
	require.NoError(t, err)
	htmlContentResponse, htmlContentBody := fetch(t, html.Resource.URL+"/content")
	require.Equal(t, 200, htmlContentResponse.StatusCode)
	require.Equal(t, wantHTML, []byte(htmlContentBody))
}

func readDocumentation(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err, path)
	return string(content)
}

func documentedCLICommands(t *testing.T, readme string) []string {
	t.Helper()
	fences := regexp.MustCompile("(?s)```(?:sh|bash)\\n(.*?)```").FindAllStringSubmatch(readme, -1)
	require.NotEmpty(t, fences, "README must contain fenced shell commands")
	commands := make([]string, 0)
	for _, fence := range fences {
		for _, line := range strings.Split(fence[1], "\n") {
			fields := strings.Fields(strings.TrimSpace(line))
			if len(fields) < 2 || fields[0] != "agent-whiteboard" {
				continue
			}
			for len(fields) > 1 && strings.HasPrefix(fields[1], "--") {
				flag := fields[1]
				fields = fields[2:]
				if flag != "--json" && !strings.Contains(flag, "=") && len(fields) > 1 {
					fields = fields[1:]
				}
			}
			if len(fields) < 2 {
				continue
			}
			command := fields[1]
			if (command == "create" || command == "update" || command == "delete" || command == "image") && len(fields) > 2 {
				command += " " + fields[2]
			}
			commands = append(commands, command)
		}
	}
	return commands
}

func requireCommandHelp(t *testing.T, commandPath []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, binaryPath, append(commandPath, "--help")...)
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	require.Contains(t, string(output), "Usage:")
}
