package integration

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestExpirationOmittedAndPermanentCreation(t *testing.T) {
	server := startServer(t, "--default-expires-in", "1", "--cleanup-interval", "20ms")
	file := writeFixture(t, "expiration.md", []byte("# expiration\n"))
	creatorContext := writeContextFixture(t, "# Expiration context\n")
	omitted := runCLIResource(t, server, "--json", "create", "markdown", "--context", creatorContext, file)
	require.False(t, omitted.Resource.Permanent)
	require.NotNil(t, omitted.Resource.ExpiresAt)
	permanent := runCLIResource(t, server, "--json", "create", "markdown", "--context", creatorContext, "--expires-in", "0", file)
	require.True(t, permanent.Resource.Permanent)
	require.Nil(t, permanent.Resource.ExpiresAt)

	waitForStatus(t, omitted.Resource.URL, http.StatusNotFound)
	require.NoDirExists(t, filepath.Join(server.Root, "whiteboards", omitted.Resource.ID))
	response, _ := fetch(t, permanent.Resource.URL)
	require.Equal(t, http.StatusOK, response.StatusCode)
	runCLIDelete(t, server, "--json", "delete", "markdown", "--", permanent.Resource.ID)
	requireCategoryEmpty(t, server.Root, "whiteboards")
}

func TestExpirationUpdatePreservesOrMovesAbsoluteDeadline(t *testing.T) {
	server := startServer(t, "--default-expires-in", "30", "--cleanup-interval", "20ms")
	first := writeFixture(t, "first.md", []byte("# first\n"))
	second := writeFixture(t, "second.md", []byte("# second\n"))
	creatorContext := writeContextFixture(t, "# Deadline context\n")
	created := runCLIResource(t, server, "--json", "create", "markdown", "--context", creatorContext, "--expires-in", "30", first)
	require.NotNil(t, created.Resource.ExpiresAt)
	preserved := runCLIResource(t, server, "--json", "update", "markdown", "--context", creatorContext, "--", created.Resource.ID, second)
	require.Equal(t, created.Resource.ExpiresAt, preserved.Resource.ExpiresAt)

	waitUntilUnix(t, time.Now().Unix()+1)
	before := time.Now()
	moved := runCLIResource(t, server, "--json", "update", "markdown", "--context", creatorContext, "--expires-in", "5", "--", created.Resource.ID, first)
	after := time.Now()
	require.NotNil(t, moved.Resource.ExpiresAt)
	require.Less(t, *moved.Resource.ExpiresAt, *created.Resource.ExpiresAt)
	require.GreaterOrEqual(t, *moved.Resource.ExpiresAt, before.Unix()+4)
	require.LessOrEqual(t, *moved.Resource.ExpiresAt, after.Unix()+5)
	runCLIDelete(t, server, "--json", "delete", "markdown", "--", created.Resource.ID)
}

func TestExpirationRejectsNegativeAndCannotReviveExpired(t *testing.T) {
	server := startServer(t, "--default-expires-in", "1", "--cleanup-interval", "20ms")
	file := writeFixture(t, "expiration.md", []byte("# expiration\n"))
	creatorContext := writeContextFixture(t, "# Expiration validation context\n")
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	stdout, stderr, err := server.RunCLI(ctx, "--json", "create", "markdown", "--context", creatorContext, "--expires-in=-1", file)
	cancel()
	require.Error(t, err)
	require.Empty(t, stdout)
	requireJSONError(t, stderr, "invalid_request")
	requireCategoryEmpty(t, server.Root, "whiteboards")

	created := runCLIResource(t, server, "--json", "create", "markdown", "--context", creatorContext, "--expires-in", "1", file)
	waitForStatus(t, created.Resource.URL, http.StatusNotFound)
	ctx, cancel = context.WithTimeout(context.Background(), integrationTimeout)
	stdout, stderr, err = server.RunCLI(ctx, "--json", "update", "markdown", "--context", creatorContext, "--expires-in", "60", "--", created.Resource.ID, file)
	cancel()
	require.Error(t, err)
	require.Empty(t, stdout)
	requireJSONError(t, stderr, "not_found")
}

func waitUntilUnix(t *testing.T, target int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()
	for time.Now().Unix() < target {
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			require.FailNow(t, "timed out waiting for wall-clock boundary")
		case <-timer.C:
		}
	}
}

func waitForStatus(t *testing.T, endpoint string, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
	defer cancel()
	client := &http.Client{Timeout: 500 * time.Millisecond}
	lastStatus := 0
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		require.NoError(t, err)
		response, err := client.Do(request)
		if err == nil {
			lastStatus = response.StatusCode
			_ = response.Body.Close()
			if lastStatus == want {
				return
			}
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			require.FailNow(t, "timed out waiting for HTTP status", "endpoint=%s want=%d last=%d", endpoint, want, lastStatus)
		case <-timer.C:
		}
	}
}
