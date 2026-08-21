package whiteboard_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/edocsss/agent-whiteboard/internal/whiteboard"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"
)

func TestViewerRendersEmbeddedAssetsWithoutTerminatingInlineElements(t *testing.T) {
	css := whiteboard.ViewerCSS()
	js := whiteboard.ViewerJS()
	viewer, err := whiteboard.NewViewer(whiteboard.ViewerConfig{CSS: css, JS: js})
	require.NoError(t, err)

	source := "# Embedded viewer\n</script><style>&\u2028\u2029"
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

	styles := findElements(document, "style", nil)
	require.Len(t, styles, 1)
	require.Equal(t, string(css), textContent(styles[0]))

	scripts := findElements(document, "script", nil)
	require.Len(t, scripts, 2)
	sourceScripts := findElements(document, "script", func(node *html.Node) bool {
		return attribute(node, "id") == "agent-whiteboard-source"
	})
	require.Len(t, sourceScripts, 1)
	require.Equal(t, "application/json", attribute(sourceScripts[0], "type"))

	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(textContent(sourceScripts[0])), &envelope))
	require.Len(t, envelope, 2)
	kindJSON, ok := envelope["kind"]
	require.True(t, ok)
	var kind string
	require.NoError(t, json.Unmarshal(kindJSON, &kind))
	require.Equal(t, "markdown", kind)
	sourceJSON, ok := envelope["source"]
	require.True(t, ok)
	var exactSource string
	require.NoError(t, json.Unmarshal(sourceJSON, &exactSource))
	require.Equal(t, source, exactSource)

	executableScripts := findElements(document, "script", func(node *html.Node) bool {
		return attribute(node, "id") != "agent-whiteboard-source"
	})
	require.Len(t, executableScripts, 1)
	require.Empty(t, attribute(executableScripts[0], "type"))
	require.Equal(t, string(js), textContent(executableScripts[0]))
	for _, script := range scripts {
		require.Empty(t, attribute(script, "src"))
	}
	require.Empty(t, findElements(document, "link", func(node *html.Node) bool {
		return strings.EqualFold(attribute(node, "rel"), "stylesheet") || attribute(node, "href") != ""
	}))

	raw := output.String()
	require.Contains(t, raw, `\u003c/script\u003e\u003cstyle\u003e\u0026`)
	require.Contains(t, raw, `\u2028`)
	require.Contains(t, raw, `\u2029`)
}
