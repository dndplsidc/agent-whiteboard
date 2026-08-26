package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/common"
	"github.com/dndplsidc/agent-whiteboard/internal/testutil"
	httpx "github.com/dndplsidc/agent-whiteboard/internal/webapi"
	"github.com/stretchr/testify/require"
)

func TestHumanSuccessWritesOnePublicURLPerLine(t *testing.T) {
	client := testutil.NewMockClient(t)
	client.EXPECT().PublicURL("/images/one").Return("https://example.test/images/one", nil).Once()
	client.EXPECT().PublicURL("/images/two").Return("https://example.test/images/two", nil).Once()
	var stdout bytes.Buffer
	require.NoError(t, writeResourceList(&stdout, false, client, []httpx.Resource{
		resource("one", "/images/one", nil), resource("two", "/images/two", int64Pointer(42)),
	}))
	require.Equal(t, "https://example.test/images/one\nhttps://example.test/images/two\n", stdout.String())
}

func TestJSONSuccessContract(t *testing.T) {
	client := testutil.NewMockClient(t)
	client.EXPECT().PublicURL("/images/id").Return("https://example.test/images/id", nil).Once()
	var stdout bytes.Buffer
	require.NoError(t, writeResource(&stdout, true, client, resource("id", "/images/id", nil)))
	require.Equal(t, "{\"schema_version\":1,\"resource\":{\"id\":\"id\",\"url\":\"https://example.test/images/id\",\"expires_at\":null,\"permanent\":true}}\n", stdout.String())
}

func TestJSONMultiImagePreservesOrder(t *testing.T) {
	client := testutil.NewMockClient(t)
	client.EXPECT().PublicURL("/images/two").Return("https://example.test/images/two", nil).Once()
	client.EXPECT().PublicURL("/images/one").Return("https://example.test/images/one", nil).Once()
	var stdout bytes.Buffer
	require.NoError(t, writeResourceList(&stdout, true, client, []httpx.Resource{
		resource("two", "/images/two", int64Pointer(22)), resource("one", "/images/one", nil),
	}))
	var body struct {
		SchemaVersion int `json:"schema_version"`
		Resources     []struct {
			ID string `json:"id"`
		} `json:"resources"`
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &body))
	require.Equal(t, 1, body.SchemaVersion)
	require.Equal(t, []string{"two", "one"}, []string{body.Resources[0].ID, body.Resources[1].ID})
}

func TestJSONImageListUsesResourcesForOneImage(t *testing.T) {
	client := testutil.NewMockClient(t)
	client.EXPECT().PublicURL("/images/id").Return("https://example.test/images/id", nil).Once()
	var stdout bytes.Buffer
	require.NoError(t, writeResourceList(&stdout, true, client, []httpx.Resource{resource("id", "/images/id", nil)}))
	require.Equal(t, "{\"schema_version\":1,\"resources\":[{\"id\":\"id\",\"url\":\"https://example.test/images/id\",\"expires_at\":null,\"permanent\":true}]}\n", stdout.String())
}

func TestErrorOutputUsesOnlyStderr(t *testing.T) {
	tests := []struct {
		name string
		json bool
		want string
	}{
		{name: "human", want: "Error: resource not found\n"},
		{name: "json", json: true, want: "{\"schema_version\":1,\"error\":{\"code\":\"not_found\",\"message\":\"resource not found\"}}\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := common.NewError(common.CodeNotFound, "resource not found", nil)
			writeCommandError(&stdout, &stderr, test.json, err)
			require.Empty(t, stdout.String())
			require.Equal(t, test.want, stderr.String())
		})
	}
}

func TestTimeoutErrorIsStable(t *testing.T) {
	require.EqualError(t, stableCommandError(context.DeadlineExceeded), "request timed out")
	require.ErrorIs(t, stableCommandError(context.DeadlineExceeded), context.DeadlineExceeded)
	require.Equal(t, "timeout", commandErrorCode(stableCommandError(context.DeadlineExceeded)))
}

func TestHumanExpirationRFC3339AndUnixFallback(t *testing.T) {
	ordinary := time.Date(2026, 7, 17, 6, 30, 0, 0, time.UTC).Unix()
	require.Equal(t, "2026-07-17T06:30:00Z", humanExpiration(&ordinary))
	tooLarge := int64(math.MaxInt64)
	require.Equal(t, "9223372036854775807", humanExpiration(&tooLarge))
	require.Equal(t, "permanent", humanExpiration(nil))
}
