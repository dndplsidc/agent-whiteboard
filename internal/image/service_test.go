package image_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/common"
	imageDomain "github.com/dndplsidc/agent-whiteboard/internal/image"
	"github.com/dndplsidc/agent-whiteboard/internal/testutil"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

const (
	testID  = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	testID2 = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	testID3 = "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"
	testID4 = "DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"
)

func TestServiceConstructorRejectsInvalidConfiguration(t *testing.T) {
	store := testutil.NewMockImageStore(t)
	clock := testutil.NewMockClock(t)
	ids := testutil.NewMockIDGenerator(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var typedNilStore *testutil.MockImageStore
	var typedNilClock *testutil.MockClock
	var typedNilIDs *testutil.MockIDGenerator

	tests := []struct {
		name              string
		store             imageDomain.Store
		clock             common.Clock
		ids               common.IDGenerator
		defaultExpiration int64
		logger            *slog.Logger
		message           string
	}{
		{name: "nil store", clock: clock, ids: ids, logger: logger, message: "store is required"},
		{name: "typed nil store", store: typedNilStore, clock: clock, ids: ids, logger: logger, message: "store is required"},
		{name: "nil clock", store: store, ids: ids, logger: logger, message: "clock is required"},
		{name: "typed nil clock", store: store, clock: typedNilClock, ids: ids, logger: logger, message: "clock is required"},
		{name: "nil id generator", store: store, clock: clock, logger: logger, message: "id generator is required"},
		{name: "typed nil id generator", store: store, clock: clock, ids: typedNilIDs, logger: logger, message: "id generator is required"},
		{name: "nil logger", store: store, clock: clock, ids: ids, message: "logger is required"},
		{name: "negative default expiration", store: store, clock: clock, ids: ids, defaultExpiration: -1, logger: logger, message: "default expiration must not be negative"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, err := imageDomain.NewService(tt.store, tt.clock, tt.ids, tt.defaultExpiration, tt.logger)
			require.Nil(t, service)
			assertDomainError(t, err, common.CodeInvalidRequest, tt.message)
		})
	}
}

func TestServiceCreateRejectsEmptyBatchBeforeDependencies(t *testing.T) {
	service, _, _, _ := newTestService(t, 0, discardLogger())

	result, err := service.CreateImages(context.Background(), imageDomain.CreateInput{})
	require.Nil(t, result)
	assertDomainError(t, err, common.CodeInvalidRequest, "at least one image is required")
}

func TestServiceCreateValidatesEntireBatchBeforeIDsOrPersistence(t *testing.T) {
	t.Run("unsupported later upload", func(t *testing.T) {
		service, _, _, _ := newTestService(t, 0, discardLogger())

		result, err := service.CreateImages(context.Background(), imageDomain.CreateInput{Images: []imageDomain.Upload{
			{Content: encodedPNG(t)},
			{Content: []byte("not an image")},
		}})
		require.Nil(t, result)
		assertDomainError(t, err, common.CodeUnsupportedMediaType, "unsupported image format")
	})

	t.Run("invalid later expiration", func(t *testing.T) {
		service, _, clock, _ := newTestService(t, 0, discardLogger())
		now := time.Unix(1_700_000_000, 0).UTC()
		clock.EXPECT().Now().Return(now).Once()

		result, err := service.CreateImages(context.Background(), imageDomain.CreateInput{Images: []imageDomain.Upload{
			{Content: encodedPNG(t)},
			{Content: encodedGIF(t), ExpiresInSeconds: int64Ptr(-1)},
		}})
		require.Nil(t, result)
		assertDomainError(t, err, common.CodeInvalidRequest, "expiration must not be negative")
	})

	t.Run("overflowing later expiration", func(t *testing.T) {
		service, _, clock, _ := newTestService(t, 0, discardLogger())
		clock.EXPECT().Now().Return(time.Unix(1_700_000_000, 0).UTC()).Once()

		result, err := service.CreateImages(context.Background(), imageDomain.CreateInput{Images: []imageDomain.Upload{
			{Content: encodedPNG(t)},
			{Content: encodedGIF(t), ExpiresInSeconds: int64Ptr(math.MaxInt64)},
		}})
		require.Nil(t, result)
		assertDomainError(t, err, common.CodeInvalidRequest, "expiration overflows unix time")
	})
}

func TestServiceCreatePersistsInSubmittedOrderWithExactContextAndFilenames(t *testing.T) {
	service, store, clock, ids := newTestService(t, 3600, discardLogger())
	ctx := context.WithValue(context.Background(), contextKey{}, "create")
	now := time.Unix(1_700_000_000, 123).UTC()
	wantExpiration := time.Unix(now.Unix()+3600, 0).UTC()
	pngContent := encodedPNG(t)
	jpegContent := encodedJPEG(t)
	zero := int64(0)
	var persisted []imageDomain.Image

	clock.EXPECT().Now().Return(now).Once()
	ids.EXPECT().NewID().Return(testID, nil).Once()
	ids.EXPECT().NewID().Return(testID2, nil).Once()
	store.EXPECT().Create(sameContext(ctx), mock.Anything).RunAndReturn(func(_ context.Context, record imageDomain.Image) error {
		persisted = append(persisted, record)
		return nil
	}).Twice()

	result, err := service.CreateImages(ctx, imageDomain.CreateInput{Images: []imageDomain.Upload{
		{Content: pngContent},
		{Content: jpegContent, ExpiresInSeconds: &zero},
	}})
	require.NoError(t, err)
	require.Len(t, persisted, 2)
	require.Equal(t, []string{testID, testID2}, []string{persisted[0].ID, persisted[1].ID})
	require.Equal(t, ".png", persisted[0].Extension)
	require.Equal(t, "image/png", persisted[0].MediaType)
	require.Equal(t, pngContent, persisted[0].Content)
	require.Equal(t, &wantExpiration, persisted[0].ExpiresAt)
	require.Equal(t, ".jpg", persisted[1].Extension)
	require.Equal(t, "image/jpeg", persisted[1].MediaType)
	require.Equal(t, jpegContent, persisted[1].Content)
	require.Nil(t, persisted[1].ExpiresAt)
	require.Equal(t, now, persisted[0].CreatedAt)
	require.Equal(t, now, persisted[0].UpdatedAt)
	require.Equal(t, []imageDomain.Result{
		{ID: testID, Filename: testID + ".png", Extension: ".png", MediaType: "image/png", CreatedAt: now, UpdatedAt: now, ExpiresAt: &wantExpiration},
		{ID: testID2, Filename: testID2 + ".jpg", Extension: ".jpg", MediaType: "image/jpeg", CreatedAt: now, UpdatedAt: now},
	}, result)
}

func TestServiceCreateRetriesOnlyCollisionsPerResource(t *testing.T) {
	service, store, clock, ids := newTestService(t, 0, discardLogger())
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()

	clock.EXPECT().Now().Return(now).Once()
	ids.EXPECT().NewID().Return(testID, nil).Once()
	ids.EXPECT().NewID().Return(testID2, nil).Once()
	ids.EXPECT().NewID().Return(testID3, nil).Once()
	store.EXPECT().Create(sameContext(ctx), mock.MatchedBy(func(record imageDomain.Image) bool { return record.ID == testID })).Return(nil).Once()
	store.EXPECT().Create(sameContext(ctx), mock.MatchedBy(func(record imageDomain.Image) bool { return record.ID == testID2 })).Return(fmt.Errorf("wrapped: %w", common.ErrIDCollision)).Once()
	store.EXPECT().Create(sameContext(ctx), mock.MatchedBy(func(record imageDomain.Image) bool { return record.ID == testID3 })).Return(nil).Once()

	result, err := service.CreateImages(ctx, imageDomain.CreateInput{Images: []imageDomain.Upload{
		{Content: encodedPNG(t)},
		{Content: encodedGIF(t)},
	}})
	require.NoError(t, err)
	require.Equal(t, []string{testID, testID3}, []string{result[0].ID, result[1].ID})
}

func TestServiceCreateRollsBackAttemptedIDOnNonCollisionError(t *testing.T) {
	service, store, clock, ids := newTestService(t, 0, discardLogger())
	ctx := context.Background()
	persistenceErr := errors.New("uncertain create")

	clock.EXPECT().Now().Return(time.Unix(1_700_000_000, 0).UTC()).Once()
	ids.EXPECT().NewID().Return(testID, nil).Once()
	store.EXPECT().Create(sameContext(ctx), mock.MatchedBy(func(record imageDomain.Image) bool {
		return record.ID == testID
	})).Return(persistenceErr).Once()
	store.EXPECT().Delete(mock.Anything, testID).Return(nil).Once()

	result, err := service.CreateImages(ctx, imageDomain.CreateInput{Images: []imageDomain.Upload{{Content: encodedPNG(t)}}})
	require.Nil(t, result)
	require.Same(t, persistenceErr, err)
}

func TestServiceCreateRollsBackPriorImagesInReverseOrderAndReturnsPersistenceError(t *testing.T) {
	service, store, clock, ids := newTestService(t, 0, discardLogger())
	ctx := context.WithValue(context.Background(), contextKey{}, "rollback")
	now := time.Unix(1_700_000_000, 0).UTC()
	persistenceErr := errors.New("write failed")
	var deleted []string

	clock.EXPECT().Now().Return(now).Once()
	for _, id := range []string{testID, testID2, testID3} {
		ids.EXPECT().NewID().Return(id, nil).Once()
	}
	store.EXPECT().Create(sameContext(ctx), mock.MatchedBy(func(record imageDomain.Image) bool { return record.ID == testID })).Return(nil).Once()
	store.EXPECT().Create(sameContext(ctx), mock.MatchedBy(func(record imageDomain.Image) bool { return record.ID == testID2 })).Return(nil).Once()
	store.EXPECT().Create(sameContext(ctx), mock.MatchedBy(func(record imageDomain.Image) bool { return record.ID == testID3 })).Return(persistenceErr).Once()
	store.EXPECT().Delete(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, id string) error {
		deleted = append(deleted, id)
		return nil
	}).Times(3)

	result, err := service.CreateImages(ctx, imageDomain.CreateInput{Images: []imageDomain.Upload{
		{Content: encodedPNG(t)},
		{Content: encodedGIF(t)},
		{Content: encodedJPEG(t)},
	}})
	require.Nil(t, result)
	require.ErrorIs(t, err, persistenceErr)
	require.Equal(t, []string{testID3, testID2, testID}, deleted)
}

func TestServiceCreateRollsBackAfterCollisionExhaustion(t *testing.T) {
	service, store, clock, ids := newTestService(t, 0, discardLogger())
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()
	terminalCollision := fmt.Errorf("third attempt: %w", common.ErrIDCollision)

	clock.EXPECT().Now().Return(now).Once()
	for _, id := range []string{testID, testID2, testID3, testID4} {
		ids.EXPECT().NewID().Return(id, nil).Once()
	}
	store.EXPECT().Create(sameContext(ctx), mock.MatchedBy(func(record imageDomain.Image) bool { return record.ID == testID })).Return(nil).Once()
	for _, id := range []string{testID2, testID3} {
		store.EXPECT().Create(sameContext(ctx), mock.MatchedBy(func(record imageDomain.Image) bool { return record.ID == id })).Return(common.ErrIDCollision).Once()
	}
	store.EXPECT().Create(sameContext(ctx), mock.MatchedBy(func(record imageDomain.Image) bool { return record.ID == testID4 })).Return(terminalCollision).Once()
	store.EXPECT().Delete(mock.Anything, testID).Return(nil).Once()

	result, err := service.CreateImages(ctx, imageDomain.CreateInput{Images: []imageDomain.Upload{
		{Content: encodedPNG(t)},
		{Content: encodedGIF(t)},
	}})
	require.Nil(t, result)
	assertDomainError(t, err, common.CodeInternal, "internal error")
	require.NotErrorIs(t, err, common.ErrIDCollision)
}

func TestServiceCreateRollbackRetainsValuesAndSurvivesParentCancellation(t *testing.T) {
	service, store, clock, ids := newTestService(t, 0, discardLogger())
	parent := context.WithValue(context.Background(), contextKey{}, "retained")
	ctx, cancel := context.WithCancel(parent)
	now := time.Unix(1_700_000_000, 0).UTC()
	persistenceErr := errors.New("write failed after cancel")
	var deleted []string

	clock.EXPECT().Now().Return(now).Once()
	ids.EXPECT().NewID().Return(testID, nil).Once()
	ids.EXPECT().NewID().Return(testID2, nil).Once()
	store.EXPECT().Create(sameContext(ctx), mock.MatchedBy(func(record imageDomain.Image) bool { return record.ID == testID })).Return(nil).Once()
	store.EXPECT().Create(sameContext(ctx), mock.MatchedBy(func(record imageDomain.Image) bool { return record.ID == testID2 })).RunAndReturn(func(context.Context, imageDomain.Image) error {
		cancel()
		return persistenceErr
	}).Once()
	store.EXPECT().Delete(mock.MatchedBy(func(cleanup context.Context) bool {
		deadline, ok := cleanup.Deadline()
		return cleanup.Value(contextKey{}) == "retained" &&
			cleanup.Err() == nil &&
			ok &&
			time.Until(deadline) > 4*time.Second &&
			time.Until(deadline) <= 5*time.Second
	}), mock.Anything).RunAndReturn(func(_ context.Context, id string) error {
		deleted = append(deleted, id)
		return nil
	}).Twice()

	result, err := service.CreateImages(ctx, imageDomain.CreateInput{Images: []imageDomain.Upload{
		{Content: encodedPNG(t)},
		{Content: encodedGIF(t)},
	}})
	require.Nil(t, result)
	require.ErrorIs(t, err, persistenceErr)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.Equal(t, []string{testID2, testID}, deleted)
}

func TestServiceCreateLogsRollbackFailureWithoutCapabilityIDs(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	service, store, clock, ids := newTestService(t, 0, logger)
	now := time.Unix(1_700_000_000, 0).UTC()
	persistenceErr := errors.New("write failed")
	rollbackErr := fmt.Errorf("cleanup failed for capability %s", testID)

	clock.EXPECT().Now().Return(now).Once()
	ids.EXPECT().NewID().Return(testID, nil).Once()
	ids.EXPECT().NewID().Return(testID2, nil).Once()
	store.EXPECT().Create(mock.Anything, mock.MatchedBy(func(record imageDomain.Image) bool { return record.ID == testID })).Return(nil).Once()
	store.EXPECT().Create(mock.Anything, mock.MatchedBy(func(record imageDomain.Image) bool { return record.ID == testID2 })).Return(persistenceErr).Once()
	store.EXPECT().Delete(mock.Anything, testID2).Return(nil).Once()
	store.EXPECT().Delete(mock.Anything, testID).Return(rollbackErr).Once()

	_, err := service.CreateImages(context.Background(), imageDomain.CreateInput{Images: []imageDomain.Upload{
		{Content: encodedPNG(t)},
		{Content: encodedGIF(t)},
	}})
	require.ErrorIs(t, err, persistenceErr)
	require.Contains(t, logs.String(), "rollback")
	require.NotContains(t, logs.String(), testID)
}

func TestServiceGetUsesExactContextAndNormalizesMissingAndExpired(t *testing.T) {
	t.Run("current", func(t *testing.T) {
		service, store, clock, _ := newTestService(t, 0, discardLogger())
		ctx := context.WithValue(context.Background(), contextKey{}, "get")
		now := time.Unix(1_700_000_000, 0).UTC()
		record := imageDomain.Image{ID: testID, Extension: ".png", MediaType: "image/png", Content: encodedPNG(t), ExpiresAt: timePtr(now.Add(time.Hour))}
		store.EXPECT().Get(sameContext(ctx), testID).Return(record, nil).Once()
		clock.EXPECT().Now().Return(now).Once()

		got, err := service.Get(ctx, testID)
		require.NoError(t, err)
		require.Equal(t, record, got)
	})

	t.Run("missing", func(t *testing.T) {
		service, store, _, _ := newTestService(t, 0, discardLogger())
		backendErr := common.NewError(common.CodeNotFound, "backend path", errors.New("secret"))
		store.EXPECT().Get(mock.Anything, testID).Return(imageDomain.Image{}, backendErr).Once()

		got, err := service.Get(context.Background(), testID)
		require.Zero(t, got)
		assertNotFound(t, err)
		require.NotErrorIs(t, err, backendErr)
	})

	t.Run("expired", func(t *testing.T) {
		service, store, clock, _ := newTestService(t, 0, discardLogger())
		now := time.Unix(1_700_000_000, 0).UTC()
		store.EXPECT().Get(mock.Anything, testID).Return(imageDomain.Image{ID: testID, ExpiresAt: &now}, nil).Once()
		clock.EXPECT().Now().Return(now).Once()

		got, err := service.Get(context.Background(), testID)
		require.Zero(t, got)
		assertNotFound(t, err)
	})
}

func TestServiceUpdateCanChangeFormatWhilePreservingIdentityCreationAndOmittedExpiration(t *testing.T) {
	service, store, clock, _ := newTestService(t, 0, discardLogger())
	ctx := context.WithValue(context.Background(), contextKey{}, "update")
	now := time.Unix(1_700_000_000, 0).UTC()
	createdAt := now.Add(-time.Hour)
	expiresAt := now.Add(time.Hour)
	current := imageDomain.Image{ID: testID, Extension: ".png", MediaType: "image/png", Content: encodedPNG(t), CreatedAt: createdAt, UpdatedAt: now.Add(-time.Minute), ExpiresAt: &expiresAt}
	newContent := encodedWebP(t)

	store.EXPECT().Get(sameContext(ctx), testID).Return(current, nil).Once()
	clock.EXPECT().Now().Return(now).Once()
	store.EXPECT().Replace(sameContext(ctx), mock.MatchedBy(func(record imageDomain.Image) bool {
		return record.ID == testID &&
			record.Extension == ".webp" &&
			record.MediaType == "image/webp" &&
			bytes.Equal(record.Content, newContent) &&
			record.CreatedAt.Equal(createdAt) &&
			record.UpdatedAt.Equal(now) &&
			record.ExpiresAt != nil && record.ExpiresAt.Equal(expiresAt)
	})).Return(nil).Once()

	result, err := service.Update(ctx, imageDomain.UpdateInput{ID: testID, Content: newContent})
	require.NoError(t, err)
	require.Equal(t, imageDomain.Result{
		ID: testID, Filename: testID + ".webp", Extension: ".webp", MediaType: "image/webp",
		CreatedAt: createdAt, UpdatedAt: now, ExpiresAt: &expiresAt,
	}, result)
}

func TestServiceUpdateNormalizesMissingExpiredAndReplaceMissing(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	tests := []struct {
		name        string
		record      imageDomain.Image
		getErr      error
		replaceErr  error
		clockCalled bool
		replaceCall bool
	}{
		{name: "missing", getErr: common.NewError(common.CodeNotFound, "backend", errors.New("secret"))},
		{name: "expired", record: imageDomain.Image{ID: testID, ExpiresAt: &now}, clockCalled: true},
		{name: "replace missing", record: imageDomain.Image{ID: testID, CreatedAt: now.Add(-time.Hour)}, replaceErr: common.NewError(common.CodeNotFound, "backend", errors.New("secret")), clockCalled: true, replaceCall: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, store, clock, _ := newTestService(t, 0, discardLogger())
			store.EXPECT().Get(mock.Anything, testID).Return(tt.record, tt.getErr).Once()
			if tt.clockCalled {
				clock.EXPECT().Now().Return(now).Once()
			}
			if tt.replaceCall {
				store.EXPECT().Replace(mock.Anything, mock.Anything).Return(tt.replaceErr).Once()
			}

			result, err := service.Update(context.Background(), imageDomain.UpdateInput{ID: testID, Content: encodedGIF(t)})
			require.Zero(t, result)
			assertNotFound(t, err)
		})
	}
}

func TestServiceUpdateRecalculatesSuppliedExpiration(t *testing.T) {
	service, store, clock, _ := newTestService(t, 0, discardLogger())
	now := time.Unix(1_700_000_000, 999).UTC()
	wantExpiration := time.Unix(now.Unix()+60, 0).UTC()
	current := imageDomain.Image{ID: testID, Extension: ".png", MediaType: "image/png", CreatedAt: now.Add(-time.Hour)}
	store.EXPECT().Get(mock.Anything, testID).Return(current, nil).Once()
	clock.EXPECT().Now().Return(now).Once()
	store.EXPECT().Replace(mock.Anything, mock.MatchedBy(func(record imageDomain.Image) bool {
		return record.ExpiresAt != nil && record.ExpiresAt.Equal(wantExpiration)
	})).Return(nil).Once()

	result, err := service.Update(context.Background(), imageDomain.UpdateInput{ID: testID, Content: encodedJPEG(t), ExpiresInSeconds: int64Ptr(60)})
	require.NoError(t, err)
	require.Equal(t, &wantExpiration, result.ExpiresAt)
}

func TestServiceDeleteUsesExactContextAndNormalizesMissingExpiredAndDeleteMissing(t *testing.T) {
	t.Run("current", func(t *testing.T) {
		service, store, clock, _ := newTestService(t, 0, discardLogger())
		ctx := context.WithValue(context.Background(), contextKey{}, "delete")
		now := time.Unix(1_700_000_000, 0).UTC()
		store.EXPECT().Get(sameContext(ctx), testID).Return(imageDomain.Image{ID: testID}, nil).Once()
		clock.EXPECT().Now().Return(now).Once()
		store.EXPECT().Delete(sameContext(ctx), testID).Return(nil).Once()
		require.NoError(t, service.Delete(ctx, testID))
	})

	now := time.Unix(1_700_000_000, 0).UTC()
	tests := []struct {
		name        string
		record      imageDomain.Image
		getErr      error
		deleteErr   error
		clockCalled bool
		deleteCall  bool
	}{
		{name: "missing", getErr: common.NewError(common.CodeNotFound, "backend", errors.New("secret"))},
		{name: "expired", record: imageDomain.Image{ID: testID, ExpiresAt: &now}, clockCalled: true},
		{name: "delete missing", record: imageDomain.Image{ID: testID}, deleteErr: common.NewError(common.CodeNotFound, "backend", errors.New("secret")), clockCalled: true, deleteCall: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, store, clock, _ := newTestService(t, 0, discardLogger())
			store.EXPECT().Get(mock.Anything, testID).Return(tt.record, tt.getErr).Once()
			if tt.clockCalled {
				clock.EXPECT().Now().Return(now).Once()
			}
			if tt.deleteCall {
				store.EXPECT().Delete(mock.Anything, testID).Return(tt.deleteErr).Once()
			}
			assertNotFound(t, service.Delete(context.Background(), testID))
		})
	}
}

func TestServiceCRUDValidatesIDsAndUpdateContentBeforeStore(t *testing.T) {
	service, _, _, _ := newTestService(t, 0, discardLogger())

	_, err := service.Get(context.Background(), "bad")
	assertDomainError(t, err, common.CodeInvalidRequest, "invalid resource id")
	_, err = service.Update(context.Background(), imageDomain.UpdateInput{ID: "bad", Content: encodedPNG(t)})
	assertDomainError(t, err, common.CodeInvalidRequest, "invalid resource id")
	_, err = service.Update(context.Background(), imageDomain.UpdateInput{ID: testID, Content: []byte("bad")})
	assertDomainError(t, err, common.CodeUnsupportedMediaType, "unsupported image format")
	err = service.Delete(context.Background(), "bad")
	assertDomainError(t, err, common.CodeInvalidRequest, "invalid resource id")
}

type contextKey struct{}

func newTestService(t *testing.T, defaultExpiration int64, logger *slog.Logger) (*imageDomain.Service, *testutil.MockImageStore, *testutil.MockClock, *testutil.MockIDGenerator) {
	t.Helper()
	store := testutil.NewMockImageStore(t)
	clock := testutil.NewMockClock(t)
	ids := testutil.NewMockIDGenerator(t)
	service, err := imageDomain.NewService(store, clock, ids, defaultExpiration, logger)
	require.NoError(t, err)
	return service, store, clock, ids
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func sameContext(want context.Context) interface{} {
	return mock.MatchedBy(func(got context.Context) bool { return got == want })
}

func assertNotFound(t *testing.T, err error) {
	t.Helper()
	assertDomainError(t, err, common.CodeNotFound, "resource not found")
	require.NotContains(t, strings.ToLower(err.Error()), "backend")
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
