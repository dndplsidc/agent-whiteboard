package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agent/attachment"
	"github.com/edocsss/agent-whiteboard/internal/agent/protocol"
	"github.com/edocsss/agent-whiteboard/internal/agent/provider"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestImageResourceUploadReadAndDeleteUsesExactAttachmentIdentity(t *testing.T) {
	images := &fakeImages{staged: attachment.Staged{ImageID: strings.Repeat("G", 32), MediaType: "image/png", Bytes: 3}, image: attachment.Image{MediaType: "image/png", Content: []byte("png")}}
	running := startServerWithImages(t, images)
	stream := running.request(t, http.MethodPost, protocol.ConnectPath, trustedOrigin, encodeCommand(t, connectCommand()))
	require.Equal(t, http.StatusOK, stream.StatusCode)
	t.Cleanup(func() { stream.Body.Close() })

	upload := imageRequest(t, running, http.MethodPost, protocol.ImagesPath, trustedOrigin, []byte("png"), true)
	require.Equal(t, http.StatusCreated, upload.StatusCode)
	require.Equal(t, trustedOrigin, upload.Header.Get("Access-Control-Allow-Origin"))
	require.Equal(t, "no-store", upload.Header.Get("Cache-Control"))
	require.JSONEq(t, `{"image_id":"GGGGGGGGGGGGGGGGGGGGGGGGGGGGGGGG","media_type":"image/png","bytes":3}`, string(mustReadAll(t, upload)))

	read := imageRequest(t, running, http.MethodGet, protocol.ImagesPath+"/"+images.staged.ImageID, trustedOrigin, nil, false)
	require.Equal(t, http.StatusOK, read.StatusCode)
	require.Equal(t, "image/png", read.Header.Get("Content-Type"))
	require.Equal(t, "png", string(mustReadAll(t, read)))

	deleted := imageRequest(t, running, http.MethodDelete, protocol.ImagesPath+"/"+images.staged.ImageID, trustedOrigin, nil, false)
	require.Equal(t, http.StatusNoContent, deleted.StatusCode)
	deleted.Body.Close()

	images.mu.Lock()
	require.Equal(t, provider.NamePi, images.stage.Provider)
	require.Equal(t, conversation, images.stage.ConversationID)
	require.Equal(t, clientID, images.stage.ClientID)
	require.Equal(t, trustedOrigin, images.stage.Origin)
	require.Equal(t, attachment.PurposeAttachment, images.stage.Purpose)
	require.Equal(t, images.staged.ImageID, images.read.ImageID)
	require.Equal(t, images.staged.ImageID, images.deleted.ImageID)
	images.mu.Unlock()
}

func TestImageResourceAcceptsInlineReferencePurpose(t *testing.T) {
	images := &fakeImages{staged: attachment.Staged{ImageID: strings.Repeat("G", 32), MediaType: "image/png", Bytes: 3}}
	running := startServerWithImages(t, images)
	stream := running.request(t, http.MethodPost, protocol.ConnectPath, trustedOrigin, encodeCommand(t, connectCommand()))
	require.Equal(t, http.StatusOK, stream.StatusCode)
	t.Cleanup(func() { stream.Body.Close() })
	request, err := http.NewRequest(http.MethodPost, running.baseURL+protocol.ImagesPath, bytes.NewReader([]byte("png")))
	require.NoError(t, err)
	request.Header.Set("Origin", trustedOrigin)
	request.Header.Set("Content-Type", "image/png")
	request.Header.Set(protocol.APIVersionHeader, protocol.APIVersion)
	request.Header.Set(protocol.ClientIDHeader, clientID)
	request.Header.Set(protocol.ConversationIDHeader, conversation)
	request.Header.Set(protocol.ProviderHeader, string(provider.NamePi))
	request.Header.Set(protocol.ImagePurposeHeader, string(attachment.PurposeInlineReference))
	response, err := running.client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, response.StatusCode)
	response.Body.Close()
	images.mu.Lock()
	require.Equal(t, attachment.PurposeInlineReference, images.stage.Purpose)
	images.mu.Unlock()
}

func TestImageResourceUsesWebSocketAttachmentIdentity(t *testing.T) {
	images := &fakeImages{staged: attachment.Staged{ImageID: strings.Repeat("G", 32), MediaType: "image/png", Bytes: 3}}
	running := startServerWithImages(t, images)
	dialer := websocket.Dialer{Subprotocols: []string{protocol.WebSocketSubprotocol}}
	header := http.Header{"Origin": []string{trustedOrigin}}
	socket, response, err := dialer.Dial("ws://"+strings.TrimPrefix(running.baseURL, "http://")+protocol.ConnectPath, header)
	require.NoError(t, err)
	if response != nil {
		response.Body.Close()
	}
	t.Cleanup(func() { socket.Close() })
	require.NoError(t, socket.WriteMessage(websocket.TextMessage, encodeCommand(t, connectCommand())))
	messageType, _, err := socket.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, messageType)

	upload := imageRequest(t, running, http.MethodPost, protocol.ImagesPath, trustedOrigin, []byte("png"), true)
	require.Equal(t, http.StatusCreated, upload.StatusCode)
	require.Equal(t, trustedOrigin, upload.Header.Get("Access-Control-Allow-Origin"))
	upload.Body.Close()
}

func TestImageResourceRejectsMissingConnectionHeadersAndMismatches(t *testing.T) {
	images := &fakeImages{staged: attachment.Staged{ImageID: strings.Repeat("G", 32), MediaType: "image/png", Bytes: 3}}
	running := startServerWithImages(t, images)

	request, err := http.NewRequest(http.MethodPost, running.baseURL+protocol.ImagesPath, bytes.NewReader([]byte("png")))
	require.NoError(t, err)
	request.Header.Set("Origin", trustedOrigin)
	request.Header.Set("Content-Type", "image/png")
	request.Header.Set(protocol.APIVersionHeader, protocol.APIVersion)
	response, err := running.client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	response.Body.Close()

	stream := running.request(t, http.MethodPost, protocol.ConnectPath, trustedOrigin, encodeCommand(t, connectCommand()))
	require.Equal(t, http.StatusOK, stream.StatusCode)
	t.Cleanup(func() { stream.Body.Close() })

	for _, mutate := range []func(*http.Request){
		func(request *http.Request) { request.Header.Set(protocol.ClientIDHeader, strings.Repeat("Z", 32)) },
		func(request *http.Request) {
			request.Header.Set(protocol.ConversationIDHeader, strings.Repeat("Z", 32))
		},
		func(request *http.Request) { request.Header.Set(protocol.APIVersionHeader, "1") },
		func(request *http.Request) { request.Header.Del(protocol.ProviderHeader) },
	} {
		request := newImageRequest(t, running, http.MethodPost, protocol.ImagesPath, trustedOrigin, []byte("png"), true)
		mutate(request)
		response, err := running.client.Do(request)
		require.NoError(t, err)
		require.NotEqual(t, http.StatusCreated, response.StatusCode)
		response.Body.Close()
	}

	request = newImageRequest(t, running, http.MethodPost, protocol.ImagesPath, otherOrigin, []byte("png"), true)
	response, err = running.client.Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.Empty(t, response.Header.Get("Access-Control-Allow-Origin"))
	response.Body.Close()
}

func TestImageResourceCORSPreflightIsExactForEachMethod(t *testing.T) {
	running := startServerWithImages(t, &fakeImages{})
	stream := running.request(t, http.MethodPost, protocol.ConnectPath, trustedOrigin, encodeCommand(t, connectCommand()))
	require.Equal(t, http.StatusOK, stream.StatusCode)
	t.Cleanup(func() { stream.Body.Close() })

	for _, test := range []struct {
		path    string
		method  string
		headers string
	}{
		{protocol.ImagesPath, http.MethodPost, strings.Join(imageUploadHeaders, ", ")},
		{protocol.ImagesPath + "/" + strings.Repeat("G", 32), http.MethodGet, strings.Join(imageReadHeaders, ", ")},
		{protocol.ImagesPath + "/" + strings.Repeat("G", 32), http.MethodDelete, strings.Join(imageReadHeaders, ", ")},
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
	staged  attachment.Staged
	image   attachment.Image
	stage   attachment.StageRequest
	read    attachment.ReadRequest
	deleted attachment.DeleteRequest
}

func (images *fakeImages) Stage(_ context.Context, request attachment.StageRequest) (attachment.Staged, error) {
	content, err := io.ReadAll(request.Content)
	if err != nil {
		return attachment.Staged{}, err
	}
	request.Content = bytes.NewReader(content)
	images.mu.Lock()
	images.stage = request
	images.mu.Unlock()
	return images.staged, nil
}

func (images *fakeImages) Read(_ context.Context, request attachment.ReadRequest) (attachment.Image, error) {
	images.mu.Lock()
	images.read = request
	images.mu.Unlock()
	return images.image, nil
}

func (images *fakeImages) DeleteStaged(_ context.Context, request attachment.DeleteRequest) error {
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
	request.Header.Set(protocol.APIVersionHeader, protocol.APIVersion)
	request.Header.Set(protocol.ClientIDHeader, clientID)
	request.Header.Set(protocol.ConversationIDHeader, conversation)
	if upload {
		request.Header.Set("Content-Type", "image/png")
		request.Header.Set(protocol.ProviderHeader, string(protocol.ProviderPi))
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
