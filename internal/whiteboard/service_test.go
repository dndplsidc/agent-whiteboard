package whiteboard_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/common"
	"github.com/edocsss/agent-whiteboard/internal/testutil"
	"github.com/edocsss/agent-whiteboard/internal/whiteboard"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const (
	testID  = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	testID2 = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	testID3 = "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
)

func TestNewServiceRejectsInvalidConfiguration(t *testing.T) {
	store := testutil.NewMockWhiteboardStore(t)
	clock := testutil.NewMockClock(t)
	ids := testutil.NewMockIDGenerator(t)

	tests := []struct {
		name              string
		store             whiteboard.Store
		clock             common.Clock
		ids               common.IDGenerator
		defaultExpiration int64
		message           string
	}{
		{name: "nil store", clock: clock, ids: ids, message: "store is required"},
		{name: "nil clock", store: store, ids: ids, message: "clock is required"},
		{name: "nil id generator", store: store, clock: clock, message: "id generator is required"},
		{name: "negative default expiration", store: store, clock: clock, ids: ids, defaultExpiration: -1, message: "default expiration must not be negative"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, err := whiteboard.NewService(tt.store, tt.clock, tt.ids, tt.defaultExpiration)
			require.Nil(t, service)
			assertDomainError(t, err, common.CodeInvalidRequest, tt.message)
		})
	}
}

func TestCreateMarkdownPersistsExactDocumentAndDefaultExpiration(t *testing.T) {
	service, store, clock, ids := newTestService(t, 3600)
	ctx := context.WithValue(context.Background(), contextKey{}, "create")
	now := time.Unix(1_700_000_000, 123).UTC()
	wantExpiration := time.Unix(now.Unix()+3600, 0).UTC()
	source := []byte("# Hello\n\n世界\n")
	creatorContext := []byte("Goal: preserve this exactly.\nAssumption: 世界\n")

	clock.EXPECT().Now().Return(now).Once()
	ids.EXPECT().NewID().Return(testID, nil).Once()
	store.EXPECT().Create(sameContext(ctx), mock.MatchedBy(func(got whiteboard.Whiteboard) bool {
		return got.ID == testID &&
			got.Kind == whiteboard.KindMarkdown &&
			bytes.Equal(got.Source, source) &&
			bytes.Equal(got.Context, creatorContext) &&
			got.CreatedAt.Equal(now) &&
			got.UpdatedAt.Equal(now) &&
			got.ExpiresAt != nil && got.ExpiresAt.Equal(wantExpiration)
	})).Return(nil).Once()

	result, err := service.CreateMarkdown(ctx, whiteboard.CreateInput{Source: source, Context: creatorContext})
	require.NoError(t, err)
	require.Equal(t, whiteboard.Result{
		ID:        testID,
		Kind:      whiteboard.KindMarkdown,
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: &wantExpiration,
	}, result)
}

func TestCreateWithExplicitZeroExpirationIsPermanent(t *testing.T) {
	service, store, clock, ids := newTestService(t, 3600)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()
	zero := int64(0)

	clock.EXPECT().Now().Return(now).Once()
	ids.EXPECT().NewID().Return(testID, nil).Once()
	store.EXPECT().Create(sameContext(ctx), mock.MatchedBy(func(got whiteboard.Whiteboard) bool {
		return got.ExpiresAt == nil
	})).Return(nil).Once()

	result, err := service.CreateMarkdown(ctx, whiteboard.CreateInput{
		Source:           []byte("permanent"),
		Context:          []byte("creator context"),
		ExpiresInSeconds: &zero,
	})
	require.NoError(t, err)
	require.Nil(t, result.ExpiresAt)
}

func TestCreateMarkdownRejectsInvalidInputBeforeDependencies(t *testing.T) {
	tests := []struct {
		name    string
		input   whiteboard.CreateInput
		message string
	}{
		{
			name:    "empty source",
			input:   whiteboard.CreateInput{Context: []byte("valid")},
			message: "markdown must not be empty",
		},
		{
			name:    "invalid source UTF-8",
			input:   whiteboard.CreateInput{Source: []byte{0xff}, Context: []byte("valid")},
			message: "markdown must be UTF-8",
		},
		{
			name:    "empty context",
			input:   whiteboard.CreateInput{Source: []byte("valid")},
			message: "markdown context must not be empty",
		},
		{
			name:    "invalid context UTF-8",
			input:   whiteboard.CreateInput{Source: []byte("valid"), Context: []byte{0xff}},
			message: "markdown context must be UTF-8",
		},
		{
			name: "negative expiration",
			input: whiteboard.CreateInput{
				Source:           []byte("valid"),
				Context:          []byte("valid context"),
				ExpiresInSeconds: int64Ptr(-1),
			},
			message: "expiration must not be negative",
		},
		{
			name: "overflowing expiration",
			input: whiteboard.CreateInput{
				Source:           []byte("valid"),
				Context:          []byte("valid context"),
				ExpiresInSeconds: int64Ptr(math.MaxInt64),
			},
			message: "expiration overflows unix time",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, _, clock, _ := newTestService(t, 3600)
			if tt.name == "negative expiration" || tt.name == "overflowing expiration" {
				clock.EXPECT().Now().Return(time.Unix(1_700_000_000, 0).UTC()).Once()
			}

			result, err := service.CreateMarkdown(context.Background(), tt.input)
			require.Zero(t, result)
			assertDomainError(t, err, common.CodeInvalidRequest, tt.message)
		})
	}
}

func TestCreateHTMLPreservesValidOriginalBytes(t *testing.T) {
	service, store, clock, ids := newTestService(t, 0)
	ctx := context.WithValue(context.Background(), contextKey{}, "html")
	now := time.Unix(1_700_000_000, 0).UTC()
	source := []byte(`<!DoCtYpE html><HTML><HEAD><TITLE>X</TITLE><LiNk ReL="alternate icon"><STYLE>body{color:red}</STYLE></HEAD><BODY><SCRIPT>document.body.dataset.ok="yes"</SCRIPT></BODY></HTML>`)

	clock.EXPECT().Now().Return(now).Once()
	ids.EXPECT().NewID().Return(testID, nil).Once()
	store.EXPECT().Create(sameContext(ctx), mock.MatchedBy(func(got whiteboard.Whiteboard) bool {
		return got.Kind == whiteboard.KindHTML && bytes.Equal(got.Source, source)
	})).Return(nil).Once()

	result, err := service.CreateHTML(ctx, whiteboard.CreateInput{Source: source})
	require.NoError(t, err)
	require.Equal(t, whiteboard.KindHTML, result.Kind)
}

func TestCreateHTMLRejectsContextBeforeDependencies(t *testing.T) {
	service, _, _, _ := newTestService(t, 0)

	result, err := service.CreateHTML(context.Background(), whiteboard.CreateInput{
		Source:  []byte(`<!doctype html><html><head></head><body></body></html>`),
		Context: []byte("not supported"),
	})
	require.Zero(t, result)
	assertDomainError(t, err, common.CodeInvalidRequest, "html must not include context")
}

func TestCreateHTMLRejectsUnsafeOrIncompleteDocuments(t *testing.T) {
	tests := []struct {
		name    string
		source  []byte
		message string
	}{
		{name: "invalid UTF-8", source: []byte{0xff}, message: "html must be UTF-8"},
		{name: "missing doctype", source: []byte(`<html><head></head><body></body></html>`), message: "html must include a doctype"},
		{name: "missing html", source: []byte(`<!doctype html><head></head><body></body>`), message: "html must include an html element"},
		{name: "missing head", source: []byte(`<!doctype html><html><body></body></html>`), message: "html must include a head element"},
		{name: "missing body", source: []byte(`<!doctype html><html><head></head></html>`), message: "html must include a body element"},
		{name: "script src", source: []byte(`<!doctype html><html><head></head><body><script src="x.js"></script></body></html>`), message: "html must not include scripts with src"},
		{name: "case-insensitive script attribute", source: []byte(`<!doctype html><html><head></head><body><SCRIPT SrC=""></SCRIPT></body></html>`), message: "html must not include scripts with src"},
		{name: "stylesheet link", source: []byte(`<!doctype html><html><head><link rel="stylesheet"></head><body></body></html>`), message: "html must not include stylesheet links"},
		{name: "case-insensitive stylesheet value", source: []byte(`<!doctype html><html><head><LINK REL="StyleSheet"></head><body></body></html>`), message: "html must not include stylesheet links"},
		{name: "stylesheet among whitespace-separated tokens", source: []byte("<!doctype html><html><head><link rel=\"alternate\tSTYLESHEET icon\"></head><body></body></html>"), message: "html must not include stylesheet links"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, _, _, _ := newTestService(t, 0)
			result, err := service.CreateHTML(context.Background(), whiteboard.CreateInput{Source: tt.source})
			require.Zero(t, result)
			assertDomainError(t, err, common.CodeInvalidRequest, tt.message)
		})
	}
}

func TestCreateRetriesOnlyIDCollisions(t *testing.T) {
	t.Run("wrapped collision retries then succeeds", func(t *testing.T) {
		service, store, clock, ids := newTestService(t, 0)
		ctx := context.Background()
		now := time.Unix(1_700_000_000, 0).UTC()

		clock.EXPECT().Now().Return(now).Once()
		ids.EXPECT().NewID().Return(testID, nil).Once()
		ids.EXPECT().NewID().Return(testID2, nil).Once()
		store.EXPECT().Create(sameContext(ctx), mock.MatchedBy(func(got whiteboard.Whiteboard) bool {
			return got.ID == testID
		})).Return(fmt.Errorf("wrapped: %w", common.ErrIDCollision)).Once()
		store.EXPECT().Create(sameContext(ctx), mock.MatchedBy(func(got whiteboard.Whiteboard) bool {
			return got.ID == testID2
		})).Return(nil).Once()

		result, err := service.CreateMarkdown(ctx, whiteboard.CreateInput{Source: []byte("valid"), Context: []byte("valid context")})
		require.NoError(t, err)
		require.Equal(t, testID2, result.ID)
	})

	t.Run("uncertain create returns generated resource identity", func(t *testing.T) {
		service, store, clock, ids := newTestService(t, 0)
		ctx := context.Background()
		now := time.Unix(1_700_000_000, 0).UTC()
		storeErr := uncertainCreateTestError{cause: errors.New("rollback could not be verified")}

		clock.EXPECT().Now().Return(now).Once()
		ids.EXPECT().NewID().Return(testID, nil).Once()
		store.EXPECT().Create(sameContext(ctx), mock.Anything).Return(storeErr).Once()

		result, err := service.CreateMarkdown(ctx, whiteboard.CreateInput{Source: []byte("valid"), Context: []byte("valid context")})
		require.ErrorIs(t, err, storeErr.cause)
		require.Equal(t, testID, result.ID)
		require.Equal(t, whiteboard.KindMarkdown, result.Kind)
		require.Equal(t, now, result.CreatedAt)
	})

	t.Run("non-collision returns immediately", func(t *testing.T) {
		service, store, clock, ids := newTestService(t, 0)
		ctx := context.Background()
		now := time.Unix(1_700_000_000, 0).UTC()
		storeErr := errors.New("disk unavailable")

		clock.EXPECT().Now().Return(now).Once()
		ids.EXPECT().NewID().Return(testID, nil).Once()
		store.EXPECT().Create(sameContext(ctx), mock.Anything).Return(storeErr).Once()

		_, err := service.CreateMarkdown(ctx, whiteboard.CreateInput{Source: []byte("valid"), Context: []byte("valid context")})
		require.ErrorIs(t, err, storeErr)
	})
}

type uncertainCreateTestError struct{ cause error }

func (err uncertainCreateTestError) Error() string          { return err.cause.Error() }
func (err uncertainCreateTestError) Unwrap() error          { return err.cause }
func (err uncertainCreateTestError) ResourceMayExist() bool { return true }

func TestCreateStopsAfterThreeIDCollisions(t *testing.T) {
	service, store, clock, ids := newTestService(t, 0)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()

	clock.EXPECT().Now().Return(now).Once()
	for _, id := range []string{testID, testID2, testID3} {
		ids.EXPECT().NewID().Return(id, nil).Once()
		store.EXPECT().Create(sameContext(ctx), mock.MatchedBy(func(got whiteboard.Whiteboard) bool {
			return got.ID == id
		})).Return(common.ErrIDCollision).Once()
	}

	result, err := service.CreateMarkdown(ctx, whiteboard.CreateInput{Source: []byte("valid"), Context: []byte("valid context")})
	require.Zero(t, result)
	assertDomainError(t, err, common.CodeInternal, "internal error")
	require.NotErrorIs(t, err, common.ErrIDCollision)
}

func TestGetReturnsCurrentWhiteboardWithExactContext(t *testing.T) {
	service, store, clock, _ := newTestService(t, 0)
	ctx := context.WithValue(context.Background(), contextKey{}, "get")
	now := time.Unix(1_700_000_000, 0).UTC()
	record := whiteboard.Whiteboard{
		ID:        testID,
		Kind:      whiteboard.KindHTML,
		Source:    []byte("<!doctype html><html><head></head><body></body></html>"),
		CreatedAt: now.Add(-time.Hour),
		UpdatedAt: now.Add(-time.Minute),
		ExpiresAt: timePtr(now.Add(time.Hour)),
	}

	store.EXPECT().Get(sameContext(ctx), testID).Return(record, nil).Once()
	clock.EXPECT().Now().Return(now).Once()

	got, err := service.Get(ctx, testID)
	require.NoError(t, err)
	require.Equal(t, record, got)
}

func TestGetAllowsLegacyMarkdownWithoutContext(t *testing.T) {
	service, store, clock, _ := newTestService(t, 0)
	now := time.Unix(1_700_000_000, 0).UTC()
	legacy := whiteboard.Whiteboard{
		ID: testID, Kind: whiteboard.KindMarkdown, Source: []byte("legacy markdown"), Context: nil,
	}
	store.EXPECT().Get(mock.Anything, testID).Return(legacy, nil).Once()
	clock.EXPECT().Now().Return(now).Once()

	got, err := service.Get(context.Background(), testID)
	require.NoError(t, err)
	require.Equal(t, legacy, got)
	require.Empty(t, got.Context)
}

func TestGetRejectsMalformedIDBeforeStore(t *testing.T) {
	service, _, _, _ := newTestService(t, 0)
	got, err := service.Get(context.Background(), "bad")
	require.Zero(t, got)
	assertDomainError(t, err, common.CodeInvalidRequest, "invalid resource id")
}

func TestGetMapsMissingAndExpiredToSameNotFound(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		service, store, _, _ := newTestService(t, 0)
		backendErr := common.NewError(common.CodeNotFound, "secret backend path", errors.New("details"))
		store.EXPECT().Get(mock.Anything, testID).Return(whiteboard.Whiteboard{}, backendErr).Once()

		got, err := service.Get(context.Background(), testID)
		require.Zero(t, got)
		assertNotFound(t, err)
		require.NotErrorIs(t, err, backendErr)
	})

	t.Run("expired", func(t *testing.T) {
		service, store, clock, _ := newTestService(t, 0)
		now := time.Unix(1_700_000_000, 0).UTC()
		record := whiteboard.Whiteboard{ID: testID, Kind: whiteboard.KindMarkdown, ExpiresAt: &now}
		store.EXPECT().Get(mock.Anything, testID).Return(record, nil).Once()
		clock.EXPECT().Now().Return(now).Once()

		got, err := service.Get(context.Background(), testID)
		require.Zero(t, got)
		assertNotFound(t, err)
	})
}

func TestGetReturnsNonNotFoundStoreError(t *testing.T) {
	service, store, _, _ := newTestService(t, 0)
	storeErr := common.NewError(common.CodeStorageUnavailable, "storage unavailable", errors.New("disk"))
	store.EXPECT().Get(mock.Anything, testID).Return(whiteboard.Whiteboard{}, storeErr).Once()

	_, err := service.Get(context.Background(), testID)
	require.Same(t, storeErr, err)
}

func TestUpdateReplacesLegacyMarkdownWithExactPairedContent(t *testing.T) {
	service, store, clock, _ := newTestService(t, 0)
	ctx := context.WithValue(context.Background(), contextKey{}, "update")
	now := time.Unix(1_700_000_000, 0).UTC()
	createdAt := now.Add(-2 * time.Hour)
	currentExpiration := now.Add(time.Hour)
	current := whiteboard.Whiteboard{
		ID:        testID,
		Kind:      whiteboard.KindMarkdown,
		Source:    []byte("legacy source without creator context"),
		Context:   nil,
		CreatedAt: createdAt,
		UpdatedAt: now.Add(-time.Second),
		ExpiresAt: &currentExpiration,
	}
	newSource := []byte("latest write wins")
	newContext := []byte("new exact creator context\n")

	store.EXPECT().Get(sameContext(ctx), testID).Return(current, nil).Once()
	clock.EXPECT().Now().Return(now).Once()
	store.EXPECT().Replace(sameContext(ctx), mock.MatchedBy(func(got whiteboard.Whiteboard) bool {
		return got.ID == testID &&
			got.Kind == whiteboard.KindMarkdown &&
			bytes.Equal(got.Source, newSource) &&
			bytes.Equal(got.Context, newContext) &&
			got.CreatedAt.Equal(createdAt) &&
			got.UpdatedAt.Equal(now) &&
			got.ExpiresAt != nil && got.ExpiresAt.Equal(currentExpiration)
	})).Return(nil).Once()

	result, err := service.Update(ctx, whiteboard.UpdateInput{
		ID:      testID,
		Kind:    whiteboard.KindMarkdown,
		Source:  newSource,
		Context: newContext,
	})
	require.NoError(t, err)
	require.Equal(t, whiteboard.Result{
		ID:        testID,
		Kind:      whiteboard.KindMarkdown,
		CreatedAt: createdAt,
		UpdatedAt: now,
		ExpiresAt: &currentExpiration,
	}, result)
}

func TestUpdateRecalculatesSuppliedExpiration(t *testing.T) {
	service, store, clock, _ := newTestService(t, 0)
	now := time.Unix(1_700_000_000, 987).UTC()
	wantExpiration := time.Unix(now.Unix()+60, 0).UTC()
	current := whiteboard.Whiteboard{ID: testID, Kind: whiteboard.KindMarkdown, CreatedAt: now.Add(-time.Hour)}

	store.EXPECT().Get(mock.Anything, testID).Return(current, nil).Once()
	clock.EXPECT().Now().Return(now).Once()
	store.EXPECT().Replace(mock.Anything, mock.MatchedBy(func(got whiteboard.Whiteboard) bool {
		return got.ExpiresAt != nil && got.ExpiresAt.Equal(wantExpiration)
	})).Return(nil).Once()

	result, err := service.Update(context.Background(), whiteboard.UpdateInput{
		ID:               testID,
		Kind:             whiteboard.KindMarkdown,
		Source:           []byte("updated"),
		Context:          []byte("updated context"),
		ExpiresInSeconds: int64Ptr(60),
	})
	require.NoError(t, err)
	require.Equal(t, &wantExpiration, result.ExpiresAt)
}

func TestUpdateMapsMissingExpiredAndWrongKindToSameNotFound(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	tests := []struct {
		name      string
		record    whiteboard.Whiteboard
		getErr    error
		clockCall bool
	}{
		{
			name:   "missing",
			getErr: common.NewError(common.CodeNotFound, "backend detail", errors.New("secret")),
		},
		{
			name:      "expired",
			record:    whiteboard.Whiteboard{ID: testID, Kind: whiteboard.KindMarkdown, ExpiresAt: &now},
			clockCall: true,
		},
		{
			name:      "wrong kind",
			record:    whiteboard.Whiteboard{ID: testID, Kind: whiteboard.KindHTML},
			clockCall: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, store, clock, _ := newTestService(t, 0)
			store.EXPECT().Get(mock.Anything, testID).Return(tt.record, tt.getErr).Once()
			if tt.clockCall {
				clock.EXPECT().Now().Return(now).Once()
			}

			result, err := service.Update(context.Background(), whiteboard.UpdateInput{
				ID:      testID,
				Kind:    whiteboard.KindMarkdown,
				Source:  []byte("valid"),
				Context: []byte("valid context"),
			})
			require.Zero(t, result)
			assertNotFound(t, err)
		})
	}
}

func TestUpdateValidatesIDKindSourceAndContextBeforeStore(t *testing.T) {
	tests := []struct {
		name    string
		input   whiteboard.UpdateInput
		message string
	}{
		{name: "malformed ID", input: whiteboard.UpdateInput{ID: "bad", Kind: whiteboard.KindMarkdown, Source: []byte("valid"), Context: []byte("valid context")}, message: "invalid resource id"},
		{name: "unknown kind", input: whiteboard.UpdateInput{ID: testID, Kind: KindUnknown, Source: []byte("valid"), Context: []byte("valid context")}, message: "invalid whiteboard kind"},
		{name: "empty markdown", input: whiteboard.UpdateInput{ID: testID, Kind: whiteboard.KindMarkdown, Context: []byte("valid context")}, message: "markdown must not be empty"},
		{name: "invalid markdown", input: whiteboard.UpdateInput{ID: testID, Kind: whiteboard.KindMarkdown, Source: []byte{0xff}, Context: []byte("valid context")}, message: "markdown must be UTF-8"},
		{name: "empty markdown context", input: whiteboard.UpdateInput{ID: testID, Kind: whiteboard.KindMarkdown, Source: []byte("valid")}, message: "markdown context must not be empty"},
		{name: "invalid markdown context", input: whiteboard.UpdateInput{ID: testID, Kind: whiteboard.KindMarkdown, Source: []byte("valid"), Context: []byte{0xff}}, message: "markdown context must be UTF-8"},
		{name: "invalid HTML", input: whiteboard.UpdateInput{ID: testID, Kind: whiteboard.KindHTML, Source: []byte(`<html></html>`)}, message: "html must include a doctype"},
		{name: "HTML context", input: whiteboard.UpdateInput{ID: testID, Kind: whiteboard.KindHTML, Source: []byte(`<!doctype html><html><head></head><body></body></html>`), Context: []byte("not supported")}, message: "html must not include context"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, _, _, _ := newTestService(t, 0)
			result, err := service.Update(context.Background(), tt.input)
			require.Zero(t, result)
			assertDomainError(t, err, common.CodeInvalidRequest, tt.message)
		})
	}
}

func TestDeleteChecksRecordThenDeletesWithExactContext(t *testing.T) {
	service, store, clock, _ := newTestService(t, 0)
	ctx := context.WithValue(context.Background(), contextKey{}, "delete")
	now := time.Unix(1_700_000_000, 0).UTC()
	record := whiteboard.Whiteboard{ID: testID, Kind: whiteboard.KindHTML}

	store.EXPECT().Get(sameContext(ctx), testID).Return(record, nil).Once()
	clock.EXPECT().Now().Return(now).Once()
	store.EXPECT().Delete(sameContext(ctx), testID).Return(nil).Once()

	require.NoError(t, service.Delete(ctx, whiteboard.KindHTML, testID))
}

func TestDeleteAllowsLegacyMarkdownWithoutContext(t *testing.T) {
	service, store, clock, _ := newTestService(t, 0)
	now := time.Unix(1_700_000_000, 0).UTC()
	legacy := whiteboard.Whiteboard{
		ID: testID, Kind: whiteboard.KindMarkdown, Source: []byte("legacy markdown"), Context: nil,
	}
	store.EXPECT().Get(mock.Anything, testID).Return(legacy, nil).Once()
	clock.EXPECT().Now().Return(now).Once()
	store.EXPECT().Delete(mock.Anything, testID).Return(nil).Once()

	require.NoError(t, service.Delete(context.Background(), whiteboard.KindMarkdown, testID))
}

func TestDeleteMapsMissingExpiredAndWrongKindToSameNotFound(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	tests := []struct {
		name      string
		record    whiteboard.Whiteboard
		getErr    error
		clockCall bool
	}{
		{name: "missing", getErr: common.NewError(common.CodeNotFound, "backend detail", errors.New("secret"))},
		{name: "expired", record: whiteboard.Whiteboard{ID: testID, Kind: whiteboard.KindMarkdown, ExpiresAt: &now}, clockCall: true},
		{name: "wrong kind", record: whiteboard.Whiteboard{ID: testID, Kind: whiteboard.KindHTML}, clockCall: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, store, clock, _ := newTestService(t, 0)
			store.EXPECT().Get(mock.Anything, testID).Return(tt.record, tt.getErr).Once()
			if tt.clockCall {
				clock.EXPECT().Now().Return(now).Once()
			}

			err := service.Delete(context.Background(), whiteboard.KindMarkdown, testID)
			assertNotFound(t, err)
		})
	}
}

func TestDeleteValidatesKindAndIDBeforeStore(t *testing.T) {
	tests := []struct {
		name    string
		kind    whiteboard.Kind
		id      string
		message string
	}{
		{name: "malformed ID", kind: whiteboard.KindMarkdown, id: "bad", message: "invalid resource id"},
		{name: "unknown kind", kind: KindUnknown, id: testID, message: "invalid whiteboard kind"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, _, _, _ := newTestService(t, 0)
			err := service.Delete(context.Background(), tt.kind, tt.id)
			assertDomainError(t, err, common.CodeInvalidRequest, tt.message)
		})
	}
}

const KindUnknown whiteboard.Kind = "unknown"

type contextKey struct{}

func newTestService(t *testing.T, defaultExpiration int64) (*whiteboard.Service, *testutil.MockWhiteboardStore, *testutil.MockClock, *testutil.MockIDGenerator) {
	t.Helper()
	store := testutil.NewMockWhiteboardStore(t)
	clock := testutil.NewMockClock(t)
	ids := testutil.NewMockIDGenerator(t)
	service, err := whiteboard.NewService(store, clock, ids, defaultExpiration)
	require.NoError(t, err)
	return service, store, clock, ids
}

func sameContext(want context.Context) interface{} {
	return mock.MatchedBy(func(got context.Context) bool { return got == want })
}

func assertNotFound(t *testing.T, err error) {
	t.Helper()
	assertDomainError(t, err, common.CodeNotFound, "resource not found")
}

func assertDomainError(t *testing.T, err error, code common.ErrorCode, message string) {
	t.Helper()
	require.EqualError(t, err, message)
	require.True(t, common.HasCode(err, code))
}

func int64Ptr(value int64) *int64 {
	return &value
}

func timePtr(value time.Time) *time.Time {
	return &value
}
