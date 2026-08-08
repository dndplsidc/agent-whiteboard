package localapi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agentattachment"
	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/edocsss/agent-whiteboard/internal/provider"
	"github.com/stretchr/testify/require"
)

func TestImageResourceUploadReadAndDeleteUsesExactAttachmentIdentity(t *testing.T) {
	images := &fakeImages{staged: agentattachment.Staged{ImageID: strings.Repeat("G", 32), MediaType: "image/png", Bytes: 3}, image: agentattachment.Image{MediaType: "image/png", Content: []byte("png")}}
	running := startServerWithImages(t, images)
	stream := running.request(t, http.MethodPost, agentprotocol.ConnectPath, trustedOrigin, encodeCommand(t, connectCommand()))
	require.Equal(t, http.StatusOK, stream.StatusCode)
	t.Cleanup(func() { stream.Body.Close() })

	upload := imageRequest(t, running, http.MethodPost, agentprotocol.ImagesPath, trustedOrigin, []byte("png"), true)
	require.Equal(t, http.StatusCreated, upload.StatusCode)
	require.Equal(t, trustedOrigin, upload.Header.Get("Access-Control-Allow-Origin"))
	require.Equal(t, "no-store", upload.Header.Get("Cache-Control"))
	require.JSONEq(t, `{"image_id":"GGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGG","media_type":"image/png","bytes":3}`, string(mustReadAll(t, upload)))

	read := imageRequest(t, running, http.MethodGet, agentprotocol.ImagesPath+"/"+images.staged.ImageID, trustedOrigin, nil, false)
	require.Equal(t, http.StatusOK, read.StatusCode)
	require.Equal(t, "image/png", read.Header.Get("Content-Type"))
	require.Equal(t, "png", string(mustReadAll(t, read)))

	deleted := imageRequest(t, running, http.MethodDelete, agentprotocol.ImagesPath+"/"+images.staged.ImageID, trustedOrigin, nil, false)
	require.Equal(t, http.StatusNoContent, deleted.StatusCode)
	deleted.Body.Close()

	images.mu.Lock()
	require.Equal(t, provider.NamePi, images.stage.Provider)
	require.Equal(t, conversation, images.stage.ConversationID)
	require.Equal(t, clientID, images.stage.ClientID)
	require.Equal(t, trustedOrigin, images.stage.Origin)
	require.Equal(t, images.staged.ImageID, images.read.ImageID)
	require.Equal(t, images.staged.ImageID, images.deleted.ImageID)
	images.mu.Unlock()
}

func TestImageResourceRejectsMissingConnectionHeadersAndMismatches(t *testing.T) {
	images := &fakeImages{staged: agentattachment.Staged{ImageID: strings.Repeat("G", 32), MediaType: "image/png", Bytes: 3}}
	running := startServerWithImages(t, images)

	request, err := http.NewRequest(http.MethodPost, running.baseURL+agentprotocol.ImagesPath, bytes.NewReader([]byte("png")))
	require.NoError(t, err)
	request.Header.Set("Origin", trustedOrigin)
	request.Header.Set("Content-Type", "image/png")
	request.Header.Set(agentprotocol.APIVersionHeader, agentprotocol.APIVersion)
	response, err := running.client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	response.Body.Close()

	stream := running.request(t, http.MethodPost, agentprotocol.ConnectPath, trustedOrigin, encodeCommand(t, connectCommand()))
	require.Equal(t, http.StatusOK, stream.StatusCode)
	t.Cleanup(func() { stream.Body.Close() })

	for _, mutate := range []func(*http.Request){
		func(request *http.Request) { request.Header.Set(agentprotocol.ClientIDHeader, strings.Repeat("Z", 32)) },
		func(request *http.Request) {
			request.Header.Set(agentprotocol.ConversationIDHeader, strings.Repeat("Z", 32))
		},
		func(request *http.Request) { request.Header.Set(agentprotocol.APIVersionHeader, "1") },
		func(request *http.Request) { request.Header.Del(agentprotocol.ProviderHeader) },
	} {
		request := newImageRequest(t, running, http.MethodPost, agentprotocol.ImagesPath, trustedOrigin, []byte("png"), true)
		mutate(request)
		response, err := running.client.Do(request)
		require.NoError(t, err)
		require.NotEqual(t, http.StatusCreated, response.StatusCode)
		response.Body.Close()
	}

	request = newImageRequest(t, running, http.MethodPost, agentprotocol.ImagesPath, otherOrigin, []byte("png"), true)
	response, err = running.client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.Empty(t, response.Header.Get("Access-Control-Allow-Origin"))
	response.Body.Close()
}

func TestImageResourceCORSPreflightIsExactForEachMethod(t *testing.T) {
	running := startServerWithImages(t, &fakeImages{})
	stream := running.request(t, http.MethodPost, agentprotocol.ConnectPath, trustedOrigin, encodeCommand(t, connectCommand()))
	require.Equal(t, http.StatusOK, stream.StatusCode)
	t.Cleanup(func() { stream.Body.Close() })

	for _, test := range []struct {
		path    string
		method  string
		headers string
	}{
		{agentprotocol.ImagesPath, http.MethodPost, strings.Join(imageUploadHeaders, ", ")},
		{agentprotocol.ImagesPath + "/" + strings.Repeat("G", 32), http.MethodGet, strings.Join(imageReadHeaders, ", ")},
		{agentprotocol.ImagesPath + "/" + strings.Repeat("G", 32), http.MethodDelete, strings.Join(imageReadHeaders, ", ")},
	} {
		request, err := http.NewRequest(http.MethodOptions, running.baseURL+test.path, nil)
		require.NoError(t, err)
		request.Header.Set("Origin", trustedOrigin)
		request.Header.Set("Access-Control-Request-Method", test.method)
		request.Header.Set("Access-Control-Request-Headers", test.headers)
		response, err := running.client.Do(request)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, response.StatusCode)
		require.Equal(t, test.method, response.Header.Get("Access-Control-Allow-Methods"))
		response.Body.Close()
	}
}

type fakeImages struct {
	mu      sync.Mutex
	staged  agentattachment.Staged
	image   agentattachment.Image
	stage   agentattachment.StageRequest
	read    agentattachment.ReadRequest
	deleted agentattachment.DeleteRequest
}

func (images *fakeImages) Stage(_ context.Context, request agentattachment.StageRequest) (agentattachment.Staged, error) {
	content, err := io.ReadAll(request.Content)
	if err != nil {
		return agentattachment.Staged{}, err
	}
	request.Content = bytes.NewReader(content)
	images.mu.Lock()
	images.stage = request
	images.mu.Unlock()
	return images.staged, nil
}

func (images *fakeImages) Read(_ context.Context, request agentattachment.ReadRequest) (agentattachment.Image, error) {
	images.mu.Lock()
	images.read = request
	images.mu.Unlock()
	return images.image, nil
}

func (images *fakeImages) DeleteStaged(_ context.Context, request agentattachment.DeleteRequest) error {
	images.mu.Lock()
	images.deleted = request
	images.mu.Unlock()
	return nil
}

func startServerWithImages(t *testing.T, images ImageBackend) *runningServer {
	t.Helper()
	trust := &mutableTrust{}
	trust.set(trustedOrigin)
	backend := &fakeBackend{}
	server, err := Listen(Config{Port: 0, TrustSource: trust, Backend: backend, Images: images})
	require.NoError(t, err)
	server.Serve()
	running := &runningServer{server: server, trust: trust, backend: backend, baseURL: "http://" + server.Host(), client: &http.Client{Timeout: 10 * time.Second}}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		require.NoError(t, server.Close(ctx))
	})
	return running
}

func imageRequest(t *testing.T, running *runningServer, method, path, origin string, content []byte, upload bool) *http.Response {
	t.Helper()
	request := newImageRequest(t, running, method, path, origin, content, upload)
	response, err := running.client.Do(request)
	require.NoError(t, err)
	return response
}

func newImageRequest(t *testing.T, running *runningServer, method, path, origin string, content []byte, upload bool) *http.Request {
	t.Helper()
	request, err := http.NewRequest(method, running.baseURL+path, bytes.NewReader(content))
	require.NoError(t, err)
	request.Header.Set("Origin", origin)
	request.Header.Set(agentprotocol.APIVersionHeader, agentprotocol.APIVersion)
	request.Header.Set(agentprotocol.ClientIDHeader, clientID)
	request.Header.Set(agentprotocol.ConversationIDHeader, conversation)
	if upload {
		request.Header.Set("Content-Type", "image/png")
		request.Header.Set(agentprotocol.ProviderHeader, string(agentprotocol.ProviderPi))
	}
	return request
}

func mustReadAll(t *testing.T, response *http.Response) []byte {
	t.Helper()
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return content
}
