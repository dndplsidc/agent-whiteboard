package localapi

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"testing"

	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFrozenTransportLimitsAndOversizedBodyRejection(t *testing.T) {
	assert.Equal(t, 67<<20, agentprotocol.MaxContextCommandBytes)
	assert.Equal(t, 192<<10, agentprotocol.MaxOrdinaryCommandBytes)
	assert.Equal(t, 256<<10, agentprotocol.MaxEventBytes)

	running := startServer(t)
	connection, err := net.Dial("tcp4", running.server.Host())
	require.NoError(t, err)
	defer connection.Close()
	_, err = fmt.Fprintf(connection, "POST %s HTTP/1.1\r\nHost: %s\r\nOrigin: %s\r\nContent-Type: application/json\r\n%s: %s\r\nContent-Length: %d\r\n\r\n", agentprotocol.ConnectPath, running.server.Host(), trustedOrigin, agentprotocol.APIVersionHeader, agentprotocol.APIVersion, agentprotocol.MaxContextCommandBytes+1)
	require.NoError(t, err)
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusRequestEntityTooLarge, response.StatusCode)
	response.Body.Close()
}

func TestRoutesRejectQueriesAndDoNotAuthorizeFromAmbientHeaders(t *testing.T) {
	running := startServer(t)
	response := running.request(t, http.MethodGet, agentprotocol.StatusPath+"?", trustedOrigin, nil)
	assert.Equal(t, http.StatusNotFound, response.StatusCode)
	response.Body.Close()

	request, err := http.NewRequest(http.MethodPost, running.baseURL+agentprotocol.ConnectPath, nil)
	require.NoError(t, err)
	request.Header.Set("Origin", otherOrigin)
	request.Header.Set("Referer", trustedOrigin+"/board")
	request.Header.Set("Forwarded", "host="+running.server.Host())
	request.Header.Set("X-Forwarded-Host", running.server.Host())
	request.Header.Set("Authorization", "Bearer token")
	request.AddCookie(&http.Cookie{Name: "authority", Value: "token"})
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(agentprotocol.APIVersionHeader, agentprotocol.APIVersion)
	response, err = running.client.Do(request)
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, response.StatusCode)
	assert.Empty(t, response.Header.Get("Access-Control-Allow-Origin"))
	response.Body.Close()
}
