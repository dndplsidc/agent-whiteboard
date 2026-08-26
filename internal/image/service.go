package image

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/common"
)

const (
	createAttempts  = 3
	rollbackTimeout = 5 * time.Second
)

type Service struct {
	store             Store
	clock             common.Clock
	ids               common.IDGenerator
	defaultExpiration int64
	logger            *slog.Logger
}

func NewService(store Store, clock common.Clock, ids common.IDGenerator, defaultExpiration int64, logger *slog.Logger) (*Service, error) {
	switch {
	case common.IsNil(store):
		return nil, common.NewError(common.CodeInvalidRequest, "store is required", nil)
	case common.IsNil(clock):
		return nil, common.NewError(common.CodeInvalidRequest, "clock is required", nil)
	case common.IsNil(ids):
		return nil, common.NewError(common.CodeInvalidRequest, "id generator is required", nil)
	case logger == nil:
		return nil, common.NewError(common.CodeInvalidRequest, "logger is required", nil)
	case defaultExpiration < 0:
		return nil, common.NewError(common.CodeInvalidRequest, "default expiration must not be negative", nil)
	}

	return &Service{
		store:             store,
		clock:             clock,
		ids:               ids,
		defaultExpiration: defaultExpiration,
		logger:            logger,
	}, nil
}

func (s *Service) CreateImages(ctx context.Context, input CreateInput) ([]Result, error) {
	if len(input.Images) == 0 {
		return nil, common.NewError(common.CodeInvalidRequest, "at least one image is required", nil)
	}

	prepared := make([]Image, len(input.Images))
	for index, upload := range input.Images {
		extension, mediaType, err := DetectFormat(upload.Content)
		if err != nil {
			return nil, err
		}
		prepared[index] = Image{
			Extension: extension,
			MediaType: mediaType,
			Content:   upload.Content,
		}
	}

	now := s.clock.Now()
	for index, upload := range input.Images {
		expiresAt, err := common.ResolveCreateExpiration(now, s.defaultExpiration, upload.ExpiresInSeconds)
		if err != nil {
			return nil, err
		}
		prepared[index].CreatedAt = now
		prepared[index].UpdatedAt = now
		prepared[index].ExpiresAt = expiresAt
	}

	createdIDs := make([]string, 0, len(prepared))
	results := make([]Result, 0, len(prepared))
	for _, record := range prepared {
		created, err := s.createOne(ctx, record)
		if err != nil {
			if created.ID != "" {
				createdIDs = append(createdIDs, created.ID)
			}
			s.rollback(ctx, createdIDs)
			return nil, err
		}
		createdIDs = append(createdIDs, created.ID)
		results = append(results, resultFrom(created))
	}

	return results, nil
}

func (s *Service) createOne(ctx context.Context, record Image) (Image, error) {
	for attempt := 0; attempt < createAttempts; attempt++ {
		id, err := s.ids.NewID()
		if err != nil {
			return Image{}, err
		}
		record.ID = id
		if err := s.store.Create(ctx, record); err != nil {
			if errors.Is(err, common.ErrIDCollision) {
				continue
			}
			return record, err
		}
		return record, nil
	}

	return Image{}, common.NewError(common.CodeInternal, "internal error", nil)
}

func (s *Service) rollback(parent context.Context, createdIDs []string) {
	if len(createdIDs) == 0 {
		return
	}

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), rollbackTimeout)
	defer cancel()
	for index := len(createdIDs) - 1; index >= 0; index-- {
		if err := s.store.Delete(cleanupCtx, createdIDs[index]); err != nil {
			s.logger.ErrorContext(cleanupCtx, "image batch rollback failed")
		}
	}
}

func (s *Service) Get(ctx context.Context, id string) (Image, error) {
	if err := common.ValidateID(id); err != nil {
		return Image{}, err
	}

	record, err := s.store.Get(ctx, id)
	if err != nil {
		return Image{}, normalizeNotFound(err)
	}
	if common.IsExpired(s.clock.Now(), record.ExpiresAt) {
		return Image{}, notFound()
	}
	return record, nil
}

func (s *Service) Update(ctx context.Context, input UpdateInput) (Result, error) {
	if err := common.ValidateID(input.ID); err != nil {
		return Result{}, err
	}
	extension, mediaType, err := DetectFormat(input.Content)
	if err != nil {
		return Result{}, err
	}

	current, err := s.store.Get(ctx, input.ID)
	if err != nil {
		return Result{}, normalizeNotFound(err)
	}
	now := s.clock.Now()
	if common.IsExpired(now, current.ExpiresAt) {
		return Result{}, notFound()
	}
	expiresAt, err := common.ResolveUpdateExpiration(now, current.ExpiresAt, input.ExpiresInSeconds)
	if err != nil {
		return Result{}, err
	}

	replacement := Image{
		ID:        current.ID,
		Extension: extension,
		MediaType: mediaType,
		Content:   input.Content,
		CreatedAt: current.CreatedAt,
		UpdatedAt: now,
		ExpiresAt: expiresAt,
	}
	if err := s.store.Replace(ctx, replacement); err != nil {
		return Result{}, normalizeNotFound(err)
	}
	return resultFrom(replacement), nil
}

func (s *Service) Delete(ctx context.Context, id string) error {
	if err := common.ValidateID(id); err != nil {
		return err
	}

	record, err := s.store.Get(ctx, id)
	if err != nil {
		return normalizeNotFound(err)
	}
	if common.IsExpired(s.clock.Now(), record.ExpiresAt) {
		return notFound()
	}
	return normalizeNotFound(s.store.Delete(ctx, id))
}

func resultFrom(record Image) Result {
	return Result{
		ID:        record.ID,
		Filename:  record.ID + record.Extension,
		Extension: record.Extension,
		MediaType: record.MediaType,
		CreatedAt: record.CreatedAt,
		UpdatedAt: record.UpdatedAt,
		ExpiresAt: record.ExpiresAt,
	}
}

func normalizeNotFound(err error) error {
	if err != nil && common.HasCode(err, common.CodeNotFound) {
		return notFound()
	}
	return err
}

func notFound() error {
	return common.NewError(common.CodeNotFound, "resource not found", nil)
}
