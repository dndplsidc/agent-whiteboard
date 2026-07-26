package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const deterministicBoundary = "012345678901234567890123456789012345678901234567890123456789"

type multipartFile struct {
	field, name string
	content     []byte
}

func TestLimitsMarkdownPairIndependentPartsAndMultipartOverhead(t *testing.T) {
	const sourceLimit = 32
	const contextLimit = 24
	server := startServer(t,
		"--max-whiteboard-bytes", strconv.Itoa(sourceLimit),
		"--max-context-bytes", strconv.Itoa(contextLimit),
	)

	for _, test := range []struct {
		name        string
		sourceSize  int
		contextSize int
	}{
		{name: "below both limits", sourceSize: sourceLimit - 1, contextSize: contextLimit - 1},
		{name: "at both limits", sourceSize: sourceLimit, contextSize: contextLimit},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := writeFixture(t, "whiteboard.md", bytes.Repeat([]byte("m"), test.sourceSize))
			creatorContext := writeContextFixture(t, string(bytes.Repeat([]byte("c"), test.contextSize)))
			created := runCLIResource(t, server, "--json", "create", "markdown", "--context", creatorContext, source)
			runCLIDelete(t, server, "--json", "delete", "markdown", "--", created.Resource.ID)
			requireCategoryEmpty(t, server.Root, "whiteboards")
		})
	}

	validSource := bytes.Repeat([]byte("m"), sourceLimit)
	validContext := bytes.Repeat([]byte("c"), contextLimit)
	for _, test := range []struct {
		name           string
		source         []byte
		creatorContext []byte
	}{
		{name: "source above independent limit", source: append(bytes.Clone(validSource), 'm'), creatorContext: validContext},
		{name: "context above independent limit", source: validSource, creatorContext: append(bytes.Clone(validContext), 'c')},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := writeFixture(t, "whiteboard.md", test.source)
			creatorContext := writeContextFixture(t, string(test.creatorContext))
			requireCLIToolarge(t, server, "create", "markdown", "--context", creatorContext, source)
			assertDirectTooLarge(t, server.URL+"/api/v1/whiteboards/markdown", []multipartFile{
				{field: "file", name: "whiteboard.md", content: test.source},
				{field: "context", name: "context.md", content: test.creatorContext},
			})
			requireCategoryEmpty(t, server.Root, "whiteboards")
		})
	}

	t.Run("multipart overhead above allowance", func(t *testing.T) {
		assertDirectTooLarge(t, server.URL+"/api/v1/whiteboards/markdown", []multipartFile{
			{field: "file", name: strings.Repeat("s", 64<<10) + ".md", content: validSource},
			{field: "context", name: "context.md", content: validContext},
		})
		requireCategoryEmpty(t, server.Root, "whiteboards")
	})
}

func TestLimitsPerImageBelowAtAndAbove(t *testing.T) {
	base := mustDecodeBase64(t, png1x1Base64)
	contentBoundary := len(base) + 8
	requestLimit := len(base) + 1024
	server := startServer(t,
		"--max-image-bytes", strconv.Itoa(contentBoundary),
		"--max-image-request-bytes", strconv.Itoa(requestLimit),
	)

	for _, test := range []struct {
		name string
		size int
	}{
		{name: "below", size: contentBoundary - 1},
		{name: "at", size: contentBoundary},
	} {
		t.Run(test.name, func(t *testing.T) {
			content := padImage(base, test.size)
			path := writeFixture(t, "image.bin", content)
			resources := runCLIResources(t, server, "--json", "image", "upload", path)
			deleteImages(t, server, resources.Resources)
			requireCategoryEmpty(t, server.Root, "images")
		})
	}

	above := padImage(base, contentBoundary+1)
	abovePath := writeFixture(t, "image.bin", above)
	requireCLIToolarge(t, server, "image", "upload", abovePath)
	assertDirectTooLarge(t, server.URL+"/api/v1/images", []multipartFile{{field: "images", name: "image.bin", content: above}})
	requireCategoryEmpty(t, server.Root, "images")
}

func TestLimitsAggregateImagesBelowAtAndAbove(t *testing.T) {
	base := mustDecodeBase64(t, png1x1Base64)
	firstSize := len(base) + 4
	secondBoundary := len(base) + 5
	atFiles := []multipartFile{
		{field: "images", name: "one.bin", content: padImage(base, firstSize)},
		{field: "images", name: "two.bin", content: padImage(base, secondBoundary)},
	}
	atBody, _ := deterministicMultipart(t, atFiles)
	server := startServer(t,
		"--max-image-bytes", strconv.Itoa(secondBoundary+32),
		"--max-image-request-bytes", strconv.Itoa(len(atBody)),
	)

	for _, test := range []struct {
		name       string
		secondSize int
	}{
		{name: "below", secondSize: secondBoundary - 1},
		{name: "at", secondSize: secondBoundary},
	} {
		t.Run(test.name, func(t *testing.T) {
			one := writeFixture(t, "one.bin", padImage(base, firstSize))
			two := writeFixture(t, "two.bin", padImage(base, test.secondSize))
			resources := runCLIResources(t, server, "--json", "image", "upload", one, two)
			deleteImages(t, server, resources.Resources)
			requireCategoryEmpty(t, server.Root, "images")
		})
	}

	aboveFiles := []multipartFile{
		{field: "images", name: "one.bin", content: padImage(base, firstSize)},
		{field: "images", name: "two.bin", content: padImage(base, secondBoundary+1)},
	}
	one := writeFixture(t, "one.bin", aboveFiles[0].content)
	two := writeFixture(t, "two.bin", aboveFiles[1].content)
	requireCLIToolarge(t, server, "image", "upload", one, two)
	assertDirectTooLarge(t, server.URL+"/api/v1/images", aboveFiles)
	requireCategoryEmpty(t, server.Root, "images")
}

func deterministicMultipart(t *testing.T, files []multipartFile) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	require.NoError(t, writer.SetBoundary(deterministicBoundary))
	for _, file := range files {
		part, err := writer.CreateFormFile(file.field, file.name)
		require.NoError(t, err)
		_, err = part.Write(file.content)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	return body.Bytes(), writer.FormDataContentType()
}

func runCLIResources(t *testing.T, server *testServer, args ...string) cliResourcesEnvelope {
	t.Helper()
	stdout := runCLISuccess(t, server, args...)
	var envelope cliResourcesEnvelope
	require.NoError(t, json.Unmarshal([]byte(stdout), &envelope), stdout)
	require.Equal(t, 1, envelope.SchemaVersion)
	return envelope
}

func requireCLIToolarge(t *testing.T, server *testServer, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()
	stdout, stderr, err := server.RunCLI(ctx, append([]string{"--json"}, args...)...)
	require.Error(t, err)
	require.Empty(t, stdout)
	envelope := requireJSONError(t, stderr, "content_too_large")
	require.Equal(t, "content too large", envelope.Error.Message)
}

func assertDirectTooLarge(t *testing.T, endpoint string, files []multipartFile) {
	t.Helper()
	body, contentType := deterministicMultipart(t, files)
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", contentType)
	response, err := (&http.Client{Timeout: integrationTimeout}).Do(request)
	require.NoError(t, err)
	responseBody, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusRequestEntityTooLarge, response.StatusCode)
	require.JSONEq(t, `{"error":{"code":"content_too_large","message":"content too large"}}`, string(responseBody))
}

func padImage(base []byte, size int) []byte {
	return append(bytes.Clone(base), bytes.Repeat([]byte{0}, size-len(base))...)
}

func mustDecodeBase64(t *testing.T, encoded string) []byte {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	require.NoError(t, err)
	return decoded
}

func deleteImages(t *testing.T, server *testServer, resources []cliResource) {
	t.Helper()
	for _, resource := range resources {
		runCLIDelete(t, server, "--json", "image", "delete", "--", resource.ID)
	}
}
