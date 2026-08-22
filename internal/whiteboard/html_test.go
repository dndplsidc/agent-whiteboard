package whiteboard

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/edocsss/agent-whiteboard/internal/common"
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

func TestValidateHTMLExplicitIDMustIdentifyOneSourceElement(t *testing.T) {
	source := []byte(`<!doctype html><html><head></head><body><div id="same"></div><section id="same" data-agent-section aria-label="Named"></section></body></html>`)
	require.Error(t, validateHTML(source))
}
