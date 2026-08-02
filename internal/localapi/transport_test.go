package localapi

import (
	"bufio"
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStrictMutationHeadersSchemaAndAttachmentIdentity(t *testing.T) {
	running := startServer(t)
	connectBody := encodeCommand(t, connectCommand())

	request, err := http.NewRequest(http.MethodPost, running.baseURL+agentprotocol.ConnectPath, bytes.NewReader(connectBody))
	require.NoError(t, err)
	request.Header.Set("Origin", trustedOrigin)
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set(agentprotocol.APIVersionHeader, agentprotocol.APIVersion)
	response, err := running.client.Do(request)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnsupportedMediaType, response.StatusCode)
	response.Body.Close()

	request, err = http.NewRequest(http.MethodPost, running.baseURL+agentprotocol.ConnectPath, bytes.NewReader(connectBody))
	require.NoError(t, err)
	request.Header.Set("Origin", trustedOrigin)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(agentprotocol.APIVersionHeader, "2")
	response, err = running.client.Do(request)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUpgradeRequired, response.StatusCode)
	assert.NotContains(t, readJSON(t, response), string(connectBody))

	response = running.request(t, http.MethodPost, agentprotocol.ConnectPath, trustedOrigin, []byte(`{"unknown":"native secret"}`))
	assert.Equal(t, http.StatusBadRequest, response.StatusCode)
	body := readJSON(t, response)
	assert.NotContains(t, body, "native secret")

	stream := running.request(t, http.MethodPost, agentprotocol.ConnectPath, trustedOrigin, connectBody)
	require.Equal(t, http.StatusOK, stream.StatusCode)
	reader := bufio.NewReader(stream.Body)
	_, err = reader.ReadBytes('\n')
	require.NoError(t, err)

	mismatch := ordinaryCommand()
	mismatch.ClientID = strings.Repeat("G", 32)
	response = running.request(t, http.MethodPost, CommandsPath, trustedOrigin, encodeCommand(t, mismatch))
	assert.Equal(t, http.StatusForbidden, response.StatusCode)
	response.Body.Close()

	response = running.request(t, http.MethodPost, CommandsPath, otherOrigin, encodeCommand(t, ordinaryCommand()))
	assert.Equal(t, http.StatusForbidden, response.StatusCode)
	assert.Empty(t, response.Header.Get("Access-Control-Allow-Origin"))
	response.Body.Close()
	stream.Body.Close()
}

func TestWebSocketRequiresProtocolRejectsBinaryAndTimesOutFirstFrame(t *testing.T) {
	running := startServer(t)
	wsURL := url.URL{Scheme: "ws", Host: running.server.Host(), Path: agentprotocol.ConnectPath}
	header := http.Header{"Origin": []string{trustedOrigin}}

	withoutOrigin := websocket.Dialer{Subprotocols: []string{agentprotocol.WebSocketSubprotocol}}
	_, response, err := withoutOrigin.Dial(wsURL.String(), nil)
	require.Error(t, err)
	require.NotNil(t, response)
	assert.Equal(t, http.StatusForbidden, response.StatusCode)
	response.Body.Close()

	multipleOrigins := http.Header{"Origin": []string{trustedOrigin, otherOrigin}}
	_, response, err = withoutOrigin.Dial(wsURL.String(), multipleOrigins)
	require.Error(t, err)
	require.NotNil(t, response)
	assert.Equal(t, http.StatusForbidden, response.StatusCode)
	response.Body.Close()

	withoutProtocol := websocket.Dialer{}
	_, response, err = withoutProtocol.Dial(wsURL.String(), header)
	require.Error(t, err)
	require.NotNil(t, response)
	assert.Equal(t, http.StatusUpgradeRequired, response.StatusCode)
	response.Body.Close()

	dialer := websocket.Dialer{Subprotocols: []string{agentprotocol.WebSocketSubprotocol}}
	socket, _, err := dialer.Dial(wsURL.String(), header)
	require.NoError(t, err)
	require.NoError(t, socket.WriteMessage(websocket.TextMessage, encodeCommand(t, connectCommand())))
	_, _, err = socket.ReadMessage()
	require.NoError(t, err)
	require.NoError(t, socket.WriteMessage(websocket.BinaryMessage, []byte("secret")))
	_, _, err = socket.ReadMessage()
	var closeErr *websocket.CloseError
	require.ErrorAs(t, err, &closeErr)
	assert.Equal(t, websocket.CloseUnsupportedData, closeErr.Code)
	socket.Close()

	timed, _, err := dialer.Dial(wsURL.String(), header)
	require.NoError(t, err)
	_ = timed.SetReadDeadline(time.Now().Add(7 * time.Second))
	started := time.Now()
	_, _, err = timed.ReadMessage()
	require.ErrorAs(t, err, &closeErr)
	assert.GreaterOrEqual(t, time.Since(started), 4*time.Second)
	assert.Less(t, time.Since(started), 7*time.Second)
	timed.Close()
}

func TestFallbackReconnectReplacesAndDetachesOldStream(t *testing.T) {
	running := startServer(t)
	body := encodeCommand(t, connectCommand())
	first := running.request(t, http.MethodPost, agentprotocol.ConnectPath, trustedOrigin, body)
	firstReader := bufio.NewReader(first.Body)
	_, err := firstReader.ReadBytes('\n')
	require.NoError(t, err)

	second := running.request(t, http.MethodPost, agentprotocol.ConnectPath, trustedOrigin, body)
	secondReader := bufio.NewReader(second.Body)
	_, err = secondReader.ReadBytes('\n')
	require.NoError(t, err)

	finished := make(chan error, 1)
	go func() {
		_, readErr := firstReader.ReadByte()
		finished <- readErr
	}()
	select {
	case err = <-finished:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("replaced fallback stream remained attached")
	}
	first.Body.Close()
	second.Body.Close()
}

func TestTrustSourceFailureFailsClosedButStatusRemainsMinimal(t *testing.T) {
	running := startServer(t)
	running.trust.mu.Lock()
	running.trust.err = context.DeadlineExceeded
	running.trust.mu.Unlock()
	response := running.request(t, http.MethodGet, agentprotocol.StatusPath, trustedOrigin, nil)
	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, map[string]any{"available": true, "api_version": "1", "origin_trusted": false}, readJSON(t, response))
	response = running.request(t, http.MethodPost, agentprotocol.ConnectPath, trustedOrigin, encodeCommand(t, connectCommand()))
	assert.Equal(t, http.StatusForbidden, response.StatusCode)
	response.Body.Close()

	loopbackOrigin := "http://127.0.0.1:4321"
	response = running.request(t, http.MethodGet, agentprotocol.StatusPath, loopbackOrigin, nil)
	assert.Equal(t, http.StatusOK, response.StatusCode)
	assert.Equal(t, map[string]any{"available": true, "api_version": "1", "origin_trusted": true}, readJSON(t, response))
	stream := running.request(t, http.MethodPost, agentprotocol.ConnectPath, loopbackOrigin, encodeCommand(t, connectCommand()))
	assert.Equal(t, http.StatusOK, stream.StatusCode)
	stream.Body.Close()
}
