package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/catalog"
	"github.com/dndplsidc/agent-whiteboard/internal/common"
	generalconfig "github.com/dndplsidc/agent-whiteboard/internal/config"
	"github.com/dndplsidc/agent-whiteboard/internal/testutil"
	httpx "github.com/dndplsidc/agent-whiteboard/internal/webapi"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const catalogTestID = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

type mutableCatalogClock struct{ at time.Time }

func (clock *mutableCatalogClock) Now() time.Time { return clock.at }

type catalogStub struct {
	prepareErr   error
	recordErr    error
	prepareCalls int
	creations    []catalog.Creation
}

func (stub *catalogStub) Prepare(context.Context, string, catalog.Kind) error {
	stub.prepareCalls++
	return stub.prepareErr
}

func (stub *catalogStub) RecordCreation(_ context.Context, creation catalog.Creation) error {
	stub.creations = append(stub.creations, creation)
	return stub.recordErr
}

func (*catalogStub) OpenExisting(context.Context, catalog.Identity) (*catalog.Entry, bool, error) {
	return nil, false, nil
}

func (*catalogStub) List(_ context.Context, query catalog.Query) (catalog.Page, error) {
	return catalog.Page{Records: []catalog.ResultRecord{}, Limit: query.Limit, Offset: query.Offset}, nil
}

type failingOutput struct{ err error }

func (writer failingOutput) Write([]byte) (int, error) { return 0, writer.err }

func TestCreateRequiresValidatedCatalogMetadataBeforePublication(t *testing.T) {
	dir := t.TempDir()
	source := writeFixture(t, dir, "board.md", "# Source title")
	creatorContext := writeFixture(t, dir, "context.md", "creator-only context")
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "missing title", args: []string{"create", "markdown", source, "--context", creatorContext, "--summary", "Summary"}},
		{name: "blank summary", args: []string{"create", "markdown", source, "--context", creatorContext, "--title", "Title", "--summary", " \t"}},
		{name: "invalid UTF-8 title", args: []string{"create", "markdown", source, "--context", creatorContext, "--title", string([]byte{0xff}), "--summary", "Summary"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			clientCalls := 0
			stub := &catalogStub{}
			deps := validDependencies()
			deps.NewClient = func(httpx.ClientConfig) (Client, error) {
				clientCalls++
				return testutil.NewMockClient(t), nil
			}
			deps.NewCatalog = func() (Catalog, error) { return stub, nil }
			root, err := NewRoot(deps)
			require.NoError(t, err)
			root.SetArgs(test.args)
			require.Error(t, root.ExecuteContext(context.Background()))
			require.Zero(t, clientCalls)
			require.Zero(t, stub.prepareCalls)
		})
	}
}

func TestCreateRecordsMetadataWithoutChangingUploadedFiles(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	clock := &mutableCatalogClock{at: now}
	store := newCLITestCatalog(t, clock)
	dir := t.TempDir()
	source := writeFixture(t, dir, "source.md", "# Document heading\nbody")
	creatorContext := writeFixture(t, dir, "creator.md", "private creator context")
	client := testutil.NewMockClient(t)
	client.EXPECT().CreateMarkdown(mock.Anything, mock.Anything, mock.Anything, (*int64)(nil)).RunAndReturn(
		func(_ context.Context, input, contextInput httpx.File, _ *int64) (httpx.Resource, error) {
			require.True(t, fileMatches(input, "source.md", "# Document heading\nbody"))
			require.True(t, fileMatches(contextInput, "creator.md", "private creator context"))
			return resource(catalogTestID, "/whiteboards/markdown/"+catalogTestID, nil), nil
		}).Once()
	client.EXPECT().PublicURL("/whiteboards/markdown/"+catalogTestID).Return("https://example.test/whiteboards/markdown/"+catalogTestID, nil).Once()

	var stdout bytes.Buffer
	root := mustCatalogRoot(t, client, store, &stdout, io.Discard)
	root.SetArgs([]string{"--json", "--server", "HTTPS://EXAMPLE.TEST:443/", "create", "markdown", source, "--context", creatorContext, "--title", "  Catalog title\t", "--summary", "\nSearchable summary  "})
	require.NoError(t, root.ExecuteContext(context.Background()))
	require.Contains(t, stdout.String(), catalogTestID)

	page, err := store.List(context.Background(), catalog.Query{Limit: 20})
	require.NoError(t, err)
	require.Len(t, page.Records, 1)
	require.Equal(t, "https://example.test", page.Records[0].Server)
	require.Equal(t, "Catalog title", page.Records[0].Title)
	require.Equal(t, "Searchable summary", page.Records[0].Summary)
	require.Equal(t, "source.md", page.Records[0].SourceFilename)
	require.Equal(t, now.Unix(), page.Records[0].CreatedAt)
}

func TestUpdateRejectsPresentBlankMetadataBeforeMutation(t *testing.T) {
	dir := t.TempDir()
	for _, flag := range []string{"--title", "--summary"} {
		t.Run(flag, func(t *testing.T) {
			clientCalls := 0
			deps := validDependencies()
			deps.NewClient = func(httpx.ClientConfig) (Client, error) {
				clientCalls++
				return testutil.NewMockClient(t), nil
			}
			root, err := NewRoot(deps)
			require.NoError(t, err)
			root.SetArgs([]string{"update", "markdown", catalogTestID, filepath.Join(dir, "missing.md"), "--context", filepath.Join(dir, "missing-context.md"), flag, " \t"})
			require.Error(t, root.ExecuteContext(context.Background()))
			require.Zero(t, clientCalls)
		})
	}
}

func TestCreatePreflightFailureSendsNoRequest(t *testing.T) {
	stub := &catalogStub{prepareErr: errors.New("read only")}
	clientCalls := 0
	deps := validDependencies()
	deps.NewCatalog = func() (Catalog, error) { return stub, nil }
	deps.NewClient = func(httpx.ClientConfig) (Client, error) {
		clientCalls++
		return testutil.NewMockClient(t), nil
	}
	dir := t.TempDir()
	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"create", "html", writeFixture(t, dir, "board.html", "<!doctype html>"), "--context", writeFixture(t, dir, "context.md", "context"), "--title", "Title", "--summary", "Summary"})
	err = root.ExecuteContext(context.Background())
	require.ErrorContains(t, err, "no whiteboard was published")
	require.Zero(t, clientCalls)
}

func TestCatalogListIsOfflineAndBypassesPublishingConfiguration(t *testing.T) {
	clock := &mutableCatalogClock{at: time.Unix(2_000_000_000, 0).UTC()}
	store := newCLITestCatalog(t, clock)
	require.NoError(t, store.RecordCreation(context.Background(), catalog.Creation{
		Server: "https://example.test", Kind: catalog.KindHTML, ID: catalogTestID,
		URL:   "https://example.test/whiteboards/html/" + catalogTestID,
		Title: "Recharge architecture", Summary: "Local search target", SourceFilename: "board.html",
		Permanent: true, State: catalog.StateCreated,
	}))
	clientCalls, configLoads := 0, 0
	deps := validDependencies()
	deps.NewCatalog = func() (Catalog, error) { return store, nil }
	deps.NewClient = func(httpx.ClientConfig) (Client, error) {
		clientCalls++
		return nil, errors.New("must stay offline")
	}
	var stdout bytes.Buffer
	deps.Stdout = &stdout
	deps.Stderr = io.Discard
	deps.LoadConfig = func(string) (generalconfig.Config, error) {
		configLoads++
		return generalconfig.Config{}, errors.New("invalid unrelated config")
	}
	root, err := NewRoot(deps)
	require.NoError(t, err)
	root.SetArgs([]string{"--json", "--config", filepath.Join(t.TempDir(), "missing.yaml"), "--server", "not-an-origin", "catalog", "list", "--kind", "html", "--query", "recharge target", "--limit", "1", "--offset", "0"})
	require.NoError(t, root.ExecuteContext(context.Background()))
	require.Zero(t, clientCalls)
	require.Zero(t, configLoads)
	require.JSONEq(t, `{"schema_version":1,"records":[{"schema_version":1,"server":"https://example.test","kind":"html","id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","url":"https://example.test/whiteboards/html/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","title":"Recharge architecture","summary":"Local search target","source_filename":"board.html","created_at":2000000000,"updated_at":2000000000,"expires_at":null,"permanent":true,"state":"created","deleted_at":null,"expired":false}],"total":1,"limit":1,"offset":0}`, stdout.String())
}

func TestTrackedUpdateAndDeleteRefreshCatalog(t *testing.T) {
	clock := &mutableCatalogClock{at: time.Unix(2_000_000_000, 0).UTC()}
	store := newCLITestCatalog(t, clock)
	require.NoError(t, store.RecordCreation(context.Background(), catalog.Creation{
		Server: "https://example.test", Kind: catalog.KindMarkdown, ID: catalogTestID,
		URL:   "https://example.test/whiteboards/markdown/" + catalogTestID,
		Title: "Original", Summary: "Preserved summary", SourceFilename: "original.md", Permanent: true, State: catalog.StateCreated,
	}))
	createdAt := clock.at.Unix()
	clock.at = clock.at.Add(time.Hour)
	dir := t.TempDir()
	client := testutil.NewMockClient(t)
	client.EXPECT().UpdateMarkdown(mock.Anything, catalogTestID, mock.Anything, mock.Anything, (*int64)(nil)).Return(resource(catalogTestID, "/whiteboards/markdown/"+catalogTestID, nil), nil).Once()
	client.EXPECT().PublicURL("/whiteboards/markdown/"+catalogTestID).Return("https://example.test/whiteboards/markdown/"+catalogTestID, nil).Once()
	root := mustCatalogRoot(t, client, store, io.Discard, io.Discard)
	root.SetArgs([]string{"--server", "https://example.test", "update", "markdown", catalogTestID, writeFixture(t, dir, "updated.md", "updated"), "--context", writeFixture(t, dir, "context.md", "context"), "--title", "Replacement"})
	require.NoError(t, root.ExecuteContext(context.Background()))

	page, err := store.List(context.Background(), catalog.Query{Limit: 20})
	require.NoError(t, err)
	require.Equal(t, "Replacement", page.Records[0].Title)
	require.Equal(t, "Preserved summary", page.Records[0].Summary)
	require.Equal(t, "updated.md", page.Records[0].SourceFilename)
	require.Equal(t, createdAt, page.Records[0].CreatedAt)
	require.Equal(t, clock.at.Unix(), page.Records[0].UpdatedAt)

	clock.at = clock.at.Add(time.Hour)
	deleteClient := testutil.NewMockClient(t)
	deleteClient.EXPECT().DeleteWhiteboard(mock.Anything, httpx.WhiteboardMarkdown, catalogTestID).Return(nil).Once()
	deleteRoot := mustCatalogRoot(t, deleteClient, store, io.Discard, io.Discard)
	deleteRoot.SetArgs([]string{"--server", "https://example.test", "delete", "markdown", catalogTestID})
	require.NoError(t, deleteRoot.ExecuteContext(context.Background()))
	page, err = store.List(context.Background(), catalog.Query{Limit: 20})
	require.NoError(t, err)
	require.Equal(t, catalog.StateDeleted, page.Records[0].State)
	require.Equal(t, clock.at.Unix(), *page.Records[0].DeletedAt)
}

func TestUnknownUpdateWarnsWithoutEnrollment(t *testing.T) {
	clock := &mutableCatalogClock{at: time.Unix(2_000_000_000, 0).UTC()}
	store := newCLITestCatalog(t, clock)
	dir := t.TempDir()
	client := testutil.NewMockClient(t)
	client.EXPECT().UpdateHTML(mock.Anything, catalogTestID, mock.Anything, mock.Anything, (*int64)(nil)).Return(resource(catalogTestID, "/whiteboards/html/"+catalogTestID, nil), nil).Once()
	client.EXPECT().PublicURL("/whiteboards/html/"+catalogTestID).Return("https://example.test/whiteboards/html/"+catalogTestID, nil).Once()
	var stdout, stderr bytes.Buffer
	root := mustCatalogRoot(t, client, store, &stdout, &stderr)
	root.SetArgs([]string{"--json", "--server", "https://example.test", "update", "html", catalogTestID, writeFixture(t, dir, "board.html", "updated"), "--context", writeFixture(t, dir, "context.md", "context"), "--summary", "New summary"})
	require.NoError(t, root.ExecuteContext(context.Background()))
	require.Contains(t, stdout.String(), catalogTestID)
	require.JSONEq(t, `{"schema_version":1,"warning":{"code":"catalog_record_missing","message":"Whiteboard was updated, but title and summary metadata were not recorded because this board is not in the local catalog."}}`, stderr.String())
	page, err := store.List(context.Background(), catalog.Query{Limit: 20})
	require.NoError(t, err)
	require.Empty(t, page.Records)
}

func TestCreateCatalogWriteWarningPreservesRemoteSuccess(t *testing.T) {
	stub := &catalogStub{recordErr: errors.New("disk full")}
	client := testutil.NewMockClient(t)
	client.EXPECT().CreateMarkdown(mock.Anything, mock.Anything, mock.Anything, (*int64)(nil)).Return(resource(catalogTestID, "/whiteboards/markdown/"+catalogTestID, nil), nil).Once()
	client.EXPECT().PublicURL(mock.Anything).Return("https://example.test/whiteboards/markdown/"+catalogTestID, nil).Once()
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	root := mustCatalogRoot(t, client, stub, &stdout, &stderr)
	root.SetArgs([]string{"--json", "create", "markdown", writeFixture(t, dir, "board.md", "body"), "--context", writeFixture(t, dir, "context.md", "context"), "--title", "Title", "--summary", "Summary"})
	require.NoError(t, root.ExecuteContext(context.Background()))
	require.Contains(t, stdout.String(), catalogTestID)
	require.JSONEq(t, `{"schema_version":1,"warning":{"code":"catalog_write_failed","message":"Whiteboard was published, but its local catalog record could not be saved."}}`, stderr.String())
}

func TestUncertainCreateWithStdoutFailureStillRecordsAndReturnsRemoteError(t *testing.T) {
	clock := &mutableCatalogClock{at: time.Unix(2_000_000_000, 0).UTC()}
	store := newCLITestCatalog(t, clock)
	client := testutil.NewMockClient(t)
	remoteErr := common.NewError(common.CodeStorageUnavailable, "storage unavailable", nil)
	client.EXPECT().CreateMarkdown(mock.Anything, mock.Anything, mock.Anything, (*int64)(nil)).Return(resource(catalogTestID, "/whiteboards/markdown/"+catalogTestID, nil), remoteErr).Once()
	client.EXPECT().PublicURL(mock.Anything).Return("https://example.test/whiteboards/markdown/"+catalogTestID, nil).Once()
	dir := t.TempDir()
	root := mustCatalogRoot(t, client, store, failingOutput{err: errors.New("broken pipe")}, io.Discard)
	root.SetArgs([]string{"--server", "https://example.test", "create", "markdown", writeFixture(t, dir, "board.md", "body"), "--context", writeFixture(t, dir, "context.md", "context"), "--title", "Title", "--summary", "Summary"})
	err := root.ExecuteContext(context.Background())
	require.Error(t, err)
	require.True(t, common.HasCode(err, common.CodeStorageUnavailable), "error: %v", err)
	page, listErr := store.List(context.Background(), catalog.Query{Limit: 20})
	require.NoError(t, listErr)
	require.Equal(t, catalog.StateCreationUncertain, page.Records[0].State)
}

func TestCreateWarningPrecedesRemoteErrorAndWarningWriteCannotReplaceOutcome(t *testing.T) {
	remoteErr := common.NewError(common.CodeStorageUnavailable, "storage unavailable", nil)
	newUncertainClient := func(t *testing.T) Client {
		client := testutil.NewMockClient(t)
		client.EXPECT().CreateMarkdown(mock.Anything, mock.Anything, mock.Anything, (*int64)(nil)).Return(resource(catalogTestID, "/whiteboards/markdown/"+catalogTestID, nil), remoteErr).Once()
		client.EXPECT().PublicURL(mock.Anything).Return("https://example.test/whiteboards/markdown/"+catalogTestID, nil).Once()
		return client
	}
	dir := t.TempDir()
	args := []string{"--json", "--server", "https://example.test", "create", "markdown", writeFixture(t, dir, "board.md", "body"), "--context", writeFixture(t, dir, "context.md", "context"), "--title", "Title", "--summary", "Summary"}

	t.Run("warning before error envelope", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		deps := validDependencies()
		deps.Stdout, deps.Stderr = &stdout, &stderr
		deps.NewClient = func(httpx.ClientConfig) (Client, error) { return newUncertainClient(t), nil }
		deps.NewCatalog = func() (Catalog, error) { return &catalogStub{recordErr: errors.New("disk full")}, nil }
		code := run(context.Background(), &stdout, &stderr, deps.Getenv, args, deps)
		require.Equal(t, exitRemote, code)
		require.Contains(t, stdout.String(), catalogTestID)
		lines := bytes.Split(bytes.TrimSpace(stderr.Bytes()), []byte("\n"))
		require.Len(t, lines, 2)
		require.JSONEq(t, `{"schema_version":1,"warning":{"code":"catalog_write_failed","message":"Whiteboard was published, but its local catalog record could not be saved."}}`, string(lines[0]))
		require.JSONEq(t, `{"schema_version":1,"error":{"code":"storage_unavailable","message":"storage unavailable"}}`, string(lines[1]))
	})

	t.Run("warning writer failure preserves success", func(t *testing.T) {
		client := testutil.NewMockClient(t)
		client.EXPECT().CreateMarkdown(mock.Anything, mock.Anything, mock.Anything, (*int64)(nil)).Return(resource(catalogTestID, "/whiteboards/markdown/"+catalogTestID, nil), nil).Once()
		client.EXPECT().PublicURL(mock.Anything).Return("https://example.test/whiteboards/markdown/"+catalogTestID, nil).Once()
		var stdout bytes.Buffer
		root := mustCatalogRoot(t, client, &catalogStub{recordErr: errors.New("disk full")}, &stdout, failingOutput{err: errors.New("stderr closed")})
		root.SetArgs(args)
		require.NoError(t, root.ExecuteContext(context.Background()))
		require.Contains(t, stdout.String(), catalogTestID)
	})
}

func TestCatalogHumanOutputEscapesTerminalControls(t *testing.T) {
	record := catalog.ResultRecord{Record: catalog.Record{
		SchemaVersion: 1, Server: "https://example.test", Kind: catalog.KindMarkdown, ID: catalogTestID,
		URL: "https://example.test/whiteboards/markdown/" + catalogTestID, Title: "first\nsecond", Summary: "tab\tvalue",
		SourceFilename: "board.md", CreatedAt: 2_000_000_000, UpdatedAt: 2_000_000_000, Permanent: true, State: catalog.StateCreated,
	}}
	var output bytes.Buffer
	require.NoError(t, writeCatalogList(&output, false, catalog.Page{Records: []catalog.ResultRecord{record}, Total: 1, Limit: 20}))
	require.NotContains(t, output.String(), "first\nsecond")
	require.NotContains(t, output.String(), "tab\tvalue")
	require.Contains(t, output.String(), `first'\n'second`)
	require.Contains(t, output.String(), `tab'\t'value`)
}

func mustCatalogRoot(t *testing.T, client Client, local Catalog, stdout, stderr io.Writer) interfaceRoot {
	t.Helper()
	deps := validDependencies()
	deps.Stdout = stdout
	deps.Stderr = stderr
	deps.NewClient = func(httpx.ClientConfig) (Client, error) { return client, nil }
	deps.NewCatalog = func() (Catalog, error) { return local, nil }
	root, err := NewRoot(deps)
	require.NoError(t, err)
	return root
}

func newCLITestCatalog(t *testing.T, clock common.Clock) *catalog.Store {
	t.Helper()
	store, err := catalog.New(catalog.Config{Root: filepath.Join(t.TempDir(), "catalog"), Clock: clock})
	require.NoError(t, err)
	return store
}
