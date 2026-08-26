package whiteboard

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/dndplsidc/agent-whiteboard/internal/common"
	"github.com/stretchr/testify/require"
)

type declarationFixture struct {
	Cases []struct {
		Name     string `json:"name"`
		HTML     string `json:"html"`
		Mutation struct {
			Valid bool `json:"valid"`
		} `json:"mutation"`
	} `json:"cases"`
}

func TestValidateHTMLExplicitDeclarationFixture(t *testing.T) {
	content, err := os.ReadFile("testdata/html-component-declarations-v1.json")
	require.NoError(t, err)
	var fixture declarationFixture
	require.NoError(t, json.Unmarshal(content, &fixture))

	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			source := []byte("<!doctype html><html><head><title>Fixture</title></head><body>" + test.HTML + "</body></html>")
			err := validateHTML(source)
			if test.Mutation.Valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.True(t, common.HasCode(err, common.CodeInvalidRequest))
			}
		})
	}
}

func TestValidateHTMLAllowsAndPreservesLeadingUTF8BOMPrefix(t *testing.T) {
	for _, source := range [][]byte{
		[]byte("\xef\xbb\xbf<!doctype html><html><head></head><body>content</body></html>"),
		[]byte(" \n<!--before-->\n\xef\xbb\xbf \n<!doctype html><html><head></head><body>content</body></html>"),
		[]byte("\xef\xbb\xbf \n<!--after-->\n<!doctype html><html><head></head><body>content</body></html>"),
	} {
		require.NoError(t, validateHTML(source))
	}
}

func TestValidateHTMLRejectsPublisherContentBeforeExplicitHead(t *testing.T) {
	allowedPrefix := "<!doctype html>\n<!-- publisher comment -->\n<html lang='en'>\n  "
	for _, test := range []struct {
		name   string
		prefix string
	}{
		{name: "publisher script", prefix: "<script>publisher()</script>"},
		{name: "publisher element", prefix: "<meta charset='utf-8'>"},
		{name: "publisher body", prefix: "<body>publisher</body>"},
		{name: "publisher text", prefix: "publisher text"},
		{name: "closing markup", prefix: "</html>"},
		{name: "processing instruction", prefix: "<?publisher execute?>"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := []byte(allowedPrefix + test.prefix + "<head><title>Board</title></head><body>content</body></html>")
			err := validateHTML(source)
			require.Error(t, err)
			require.True(t, common.HasCode(err, common.CodeInvalidRequest))
		})
	}

	require.NoError(t, validateHTML([]byte(allowedPrefix+"<head><title>Board</title></head><body>content</body></html>")))
}

func TestValidateHTMLExplicitIDMustIdentifyOneSourceElement(t *testing.T) {
	source := []byte(`<!doctype html><html><head></head><body><div id="same"></div><section id="same" data-agent-section aria-label="Named"></section></body></html>`)
	require.Error(t, validateHTML(source))
}
