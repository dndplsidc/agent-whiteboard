package whiteboard_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/contextdigest"
	"github.com/edocsss/agent-whiteboard/internal/whiteboard"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"
)

const (
	testViewerCSS = "#agent-whiteboard-content{color:rgb(1,2,3)}"
	testViewerJS  = "globalThis.agentWhiteboardViewerLoaded=true;"
)

func TestNewViewerRejectsMissingAssets(t *testing.T) {
	tests := []struct {
		name   string
		config whiteboard.ViewerConfig
	}{
		{name: "nil assets"},
		{name: "nil CSS", config: whiteboard.ViewerConfig{JS: []byte(testViewerJS)}},
		{name: "empty CSS", config: whiteboard.ViewerConfig{CSS: []byte{}, JS: []byte(testViewerJS)}},
		{name: "nil JavaScript", config: whiteboard.ViewerConfig{CSS: []byte(testViewerCSS)}},
		{name: "empty JavaScript", config: whiteboard.ViewerConfig{CSS: []byte(testViewerCSS), JS: []byte{}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			viewer, err := whiteboard.NewViewer(tt.config)

			require.Nil(t, viewer)
			require.Error(t, err)
			require.True(t, common.HasCode(err, common.CodeInvalidRequest), "expected invalid_request, got %v", err)
		})
	}
}

func TestViewerDisabledRendersOnlySafelyEscapedSourceAndExactCSP(t *testing.T) {
	css := []byte(testViewerCSS)
	js := []byte(testViewerJS)
	viewer, err := whiteboard.NewViewer(whiteboard.ViewerConfig{CSS: css, JS: js})
	require.NoError(t, err)

	copy(css, strings.Repeat("x", len(css)))
	copy(js, strings.Repeat("y", len(js)))
	source := "# Whiteboard\n</script><tag>&\u2028\u2029"
	var output bytes.Buffer
	require.NoError(t, viewer.Render(&output, whiteboard.Whiteboard{
		ID:     testWhiteboardID,
		Kind:   whiteboard.KindMarkdown,
		Source: []byte(source),
	}))

	document, err := html.Parse(bytes.NewReader(output.Bytes()))
	require.NoError(t, err)
	require.Equal(t, 1, countNodes(document, func(node *html.Node) bool {
		return node.Type == html.DoctypeNode && strings.EqualFold(node.Data, "html")
	}))
	require.Equal(t, 1, countElements(document, "html"))
	require.Equal(t, 1, countElements(document, "head"))
	require.Equal(t, 1, countElements(document, "body"))
	titles := findElements(document, "title", nil)
	require.Len(t, titles, 1)
	require.Equal(t, "Untitled whiteboard", textContent(titles[0]))

	robots := findElements(document, "meta", func(node *html.Node) bool {
		return attribute(node, "name") == "robots"
	})
	require.Len(t, robots, 1)
	require.Equal(t, "noindex, nofollow, noarchive", attribute(robots[0], "content"))

	styles := findElements(document, "style", nil)
	require.Len(t, styles, 1)
	require.Equal(t, testViewerCSS, textContent(styles[0]))
	require.Empty(t, attribute(styles[0], "href"))

	sourceScripts := findElements(document, "script", func(node *html.Node) bool {
		return attribute(node, "id") == "agent-whiteboard-source"
	})
	require.Len(t, sourceScripts, 1)
	require.Equal(t, "application/json", attribute(sourceScripts[0], "type"))
	var payload struct {
		Markdown string `json:"markdown"`
	}
	require.NoError(t, json.Unmarshal([]byte(textContent(sourceScripts[0])), &payload))
	require.Equal(t, source, payload.Markdown)

	scripts := findElements(document, "script", nil)
	require.Len(t, scripts, 2)
	require.Equal(t, testViewerJS, textContent(scripts[1]))
	for _, script := range scripts {
		require.Empty(t, attribute(script, "src"))
	}
	require.Empty(t, findElements(document, "link", func(node *html.Node) bool {
		return strings.EqualFold(attribute(node, "rel"), "stylesheet") || attribute(node, "href") != ""
	}))

	noscript := findElements(document, "noscript", nil)
	require.Len(t, noscript, 1)
	require.NotEmpty(t, strings.TrimSpace(textContent(noscript[0])))

	raw := output.String()
	require.Contains(t, raw, `\u003c/script\u003e`)
	require.Contains(t, raw, `\u003ctag\u003e\u0026`)
	require.Contains(t, raw, `\u2028`)
	require.Contains(t, raw, `\u2029`)
	require.NotContains(t, raw, source)
	require.NotContains(t, raw, `"context"`)
	require.NotContains(t, raw, `"local_agent"`)

	digest := sha256.Sum256([]byte(testViewerJS))
	require.Equal(t,
		fmt.Sprintf("default-src 'none'; base-uri 'none'; connect-src 'none'; font-src 'none'; form-action 'none'; frame-ancestors 'none'; frame-src 'none'; img-src 'self' data:; manifest-src 'none'; media-src 'none'; object-src 'none'; script-src 'sha256-%s'; style-src 'unsafe-inline'; worker-src 'none'", base64.StdEncoding.EncodeToString(digest[:])),
		viewer.ContentSecurityPolicy(),
	)
}

func TestViewerEnabledRendersExactSafelyEscapedSourceContextAndFlag(t *testing.T) {
	viewer, err := whiteboard.NewViewer(whiteboard.ViewerConfig{
		CSS:               []byte(testViewerCSS),
		JS:                []byte(testViewerJS),
		LocalAgentEnabled: true,
	})
	require.NoError(t, err)

	source := "# source </script><source>&\u2028\u2029"
	creatorContext := "context </script><context>&\u2028\u2029"
	createdAt := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	updatedAt := createdAt.Add(time.Hour)
	expiresAt := createdAt.Add(24 * time.Hour)
	var output bytes.Buffer
	require.NoError(t, viewer.Render(&output, whiteboard.Whiteboard{
		ID: testWhiteboardID, Kind: whiteboard.KindMarkdown,
		Source: []byte(source), Context: []byte(creatorContext),
		CreatedAt: createdAt, UpdatedAt: updatedAt, ExpiresAt: &expiresAt,
	}))

	document, err := html.Parse(bytes.NewReader(output.Bytes()))
	require.NoError(t, err)
	sourceScripts := findElements(document, "script", func(node *html.Node) bool {
		return attribute(node, "id") == "agent-whiteboard-source"
	})
	require.Len(t, sourceScripts, 1)
	var payload struct {
		Markdown   string `json:"markdown"`
		Context    string `json:"context"`
		LocalAgent struct {
			Enabled       bool   `json:"enabled"`
			ContextDigest string `json:"context_digest"`
			Resource      struct {
				Kind      whiteboard.Kind `json:"kind"`
				ID        string          `json:"id"`
				CreatedAt time.Time       `json:"created_at"`
				UpdatedAt time.Time       `json:"updated_at"`
				ExpiresAt *time.Time      `json:"expires_at"`
			} `json:"resource"`
		} `json:"local_agent"`
	}
	require.NoError(t, json.Unmarshal([]byte(textContent(sourceScripts[0])), &payload))
	require.Equal(t, source, payload.Markdown)
	require.Equal(t, creatorContext, payload.Context)
	require.True(t, payload.LocalAgent.Enabled)
	require.Equal(t, contextdigest.Calculate([]byte(source), []byte(creatorContext)), payload.LocalAgent.ContextDigest)
	require.Equal(t, whiteboard.KindMarkdown, payload.LocalAgent.Resource.Kind)
	require.Equal(t, testWhiteboardID, payload.LocalAgent.Resource.ID)
	require.Equal(t, createdAt, payload.LocalAgent.Resource.CreatedAt)
	require.Equal(t, updatedAt, payload.LocalAgent.Resource.UpdatedAt)
	require.Equal(t, expiresAt, *payload.LocalAgent.Resource.ExpiresAt)
	require.Contains(t, textContent(sourceScripts[0]), `"context_digest":"`)
	require.Contains(t, textContent(sourceScripts[0]), `"resource":{"kind":"markdown"`)

	raw := output.String()
	require.Contains(t, raw, `\u003c/script\u003e`)
	require.Contains(t, raw, `\u003ccontext\u003e\u0026`)
	require.NotContains(t, raw, source)
	require.NotContains(t, raw, creatorContext)
	digest := sha256.Sum256([]byte(testViewerJS))
	require.Equal(t,
		fmt.Sprintf("default-src 'none'; base-uri 'none'; connect-src http://127.0.0.1:* ws://127.0.0.1:*; font-src 'none'; form-action 'none'; frame-ancestors 'none'; frame-src 'none'; img-src 'self' data: blob:; manifest-src 'none'; media-src 'none'; object-src 'none'; script-src 'sha256-%s'; style-src 'unsafe-inline'; worker-src 'none'", base64.StdEncoding.EncodeToString(digest[:])),
		viewer.ContentSecurityPolicy(),
	)
	require.NotContains(t, viewer.ContentSecurityPolicy(), "https:")
	require.NotContains(t, viewer.ContentSecurityPolicy(), "wss:")
}

func TestViewerEnabledPreservesLegacyEmptyContext(t *testing.T) {
	viewer, err := whiteboard.NewViewer(whiteboard.ViewerConfig{
		CSS: []byte(testViewerCSS), JS: []byte(testViewerJS), LocalAgentEnabled: true,
	})
	require.NoError(t, err)
	createdAt := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	var output bytes.Buffer
	require.NoError(t, viewer.Render(&output, whiteboard.Whiteboard{
		ID: testWhiteboardID, Kind: whiteboard.KindMarkdown, Source: []byte("legacy"), Context: nil,
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}))
	require.Contains(t, output.String(), `"markdown":"legacy","context":""`)
	require.Contains(t, output.String(), `"enabled":true,"context_digest":"`)
}

func countElements(root *html.Node, name string) int {
	return countNodes(root, func(node *html.Node) bool {
		return node.Type == html.ElementNode && node.Data == name
	})
}

func countNodes(root *html.Node, matches func(*html.Node) bool) int {
	count := 0
	walkNodes(root, func(node *html.Node) {
		if matches(node) {
			count++
		}
	})
	return count
}

func findElements(root *html.Node, name string, matches func(*html.Node) bool) []*html.Node {
	var elements []*html.Node
	walkNodes(root, func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == name && (matches == nil || matches(node)) {
			elements = append(elements, node)
		}
	})
	return elements
}

func walkNodes(root *html.Node, visit func(*html.Node)) {
	visit(root)
	for child := root.FirstChild; child != nil; child = child.NextSibling {
		walkNodes(child, visit)
	}
}

func attribute(node *html.Node, name string) string {
	for _, attr := range node.Attr {
		if attr.Key == name {
			return attr.Val
		}
	}
	return ""
}

func textContent(node *html.Node) string {
	var content strings.Builder
	walkNodes(node, func(current *html.Node) {
		if current.Type == html.TextNode {
			content.WriteString(current.Data)
		}
	})
	return content.String()
}
