package whiteboard

import (
	"context"
	"errors"

	"github.com/edocsss/agent-whiteboard/internal/common"
)

const createAttempts = 3

type Service struct {
	store             Store
	clock             common.Clock
	ids               common.IDGenerator
	defaultExpiration int64
}

func NewService(store Store, clock common.Clock, ids common.IDGenerator, defaultExpiration int64) (*Service, error) {
	switch {
	case common.IsNil(store):
		return nil, common.NewError(common.CodeInvalidRequest, "store is required", nil)
	case common.IsNil(clock):
		return nil, common.NewError(common.CodeInvalidRequest, "clock is required", nil)
	case common.IsNil(ids):
		return nil, common.NewError(common.CodeInvalidRequest, "id generator is required", nil)
	case defaultExpiration < 0:
		return nil, common.NewError(common.CodeInvalidRequest, "default expiration must not be negative", nil)
	}

	return &Service{
		store:             store,
		clock:             clock,
		ids:               ids,
		defaultExpiration: defaultExpiration,
	}, nil
}

func (s *Service) CreateMarkdown(ctx context.Context, input CreateInput) (Result, error) {
	return s.create(ctx, KindMarkdown, input)
}

func (s *Service) CreateHTML(ctx context.Context, input CreateInput) (Result, error) {
	return s.create(ctx, KindHTML, input)
}

func (s *Service) create(ctx context.Context, kind Kind, input CreateInput) (Result, error) {
	if err := validateWrite(kind, input.Source, input.Context); err != nil {
		return Result{}, err
	}

	now := s.clock.Now()
	expiresAt, err := common.ResolveCreateExpiration(now, s.defaultExpiration, input.ExpiresInSeconds)
	if err != nil {
		return Result{}, err
	}

	for attempt := 0; attempt < createAttempts; attempt++ {
		id, err := s.ids.NewID()
		if err != nil {
			return Result{}, err
		}

		record := Whiteboard{
			ID:        id,
			Kind:      kind,
			Source:    input.Source,
			Context:   input.Context,
			CreatedAt: now,
			UpdatedAt: now,
			ExpiresAt: expiresAt,
		}
		err = s.store.Create(ctx, record)
		if err == nil {
			return resultFrom(record), nil
		}
		if !errors.Is(err, common.ErrIDCollision) {
			return Result{}, err
		}
	}

	return Result{}, common.NewError(common.CodeInternal, "internal error", nil)
}

func (s *Service) Get(ctx context.Context, id string) (Whiteboard, error) {
	if err := common.ValidateID(id); err != nil {
		return Whiteboard{}, err
	}

	record, err := s.store.Get(ctx, id)
	if err != nil {
		return Whiteboard{}, normalizeNotFound(err)
	}
	if common.IsExpired(s.clock.Now(), record.ExpiresAt) {
		return Whiteboard{}, notFound()
	}
	return record, nil
}

func (s *Service) Update(ctx context.Context, input UpdateInput) (Result, error) {
	if err := common.ValidateID(input.ID); err != nil {
		return Result{}, err
	}
	if err := validateWrite(input.Kind, input.Source, input.Context); err != nil {
		return Result{}, err
	}

	current, err := s.store.Get(ctx, input.ID)
	if err != nil {
		return Result{}, normalizeNotFound(err)
	}
	now := s.clock.Now()
	if common.IsExpired(now, current.ExpiresAt) || current.Kind != input.Kind {
		return Result{}, notFound()
	}

	expiresAt, err := common.ResolveUpdateExpiration(now, current.ExpiresAt, input.ExpiresInSeconds)
	if err != nil {
		return Result{}, err
	}
	replacement := Whiteboard{
		ID:        current.ID,
		Kind:      current.Kind,
		Source:    input.Source,
		Context:   input.Context,
		CreatedAt: current.CreatedAt,
		UpdatedAt: now,
		ExpiresAt: expiresAt,
	}
	if err := s.store.Replace(ctx, replacement); err != nil {
		return Result{}, normalizeNotFound(err)
	}
	return resultFrom(replacement), nil
}

func (s *Service) Delete(ctx context.Context, kind Kind, id string) error {
	if err := common.ValidateID(id); err != nil {
		return err
	}
	if err := validateKind(kind); err != nil {
		return err
	}

	record, err := s.store.Get(ctx, id)
	if err != nil {
		return normalizeNotFound(err)
	}
	if common.IsExpired(s.clock.Now(), record.ExpiresAt) || record.Kind != kind {
		return notFound()
	}
	return normalizeNotFound(s.store.Delete(ctx, id))
}

func validateWrite(kind Kind, source, creatorContext []byte) error {
	switch kind {
	case KindMarkdown:
		if err := validateMarkdown(source); err != nil {
			return err
		}
		return validateMarkdownContext(creatorContext)
	case KindHTML:
		if err := validateHTML(source); err != nil {
			return err
		}
		if len(creatorContext) != 0 {
			return common.NewError(common.CodeInvalidRequest, "html must not include context", nil)
		}
		return nil
	default:
		return common.NewError(common.CodeInvalidRequest, "invalid whiteboard kind", nil)
	}
}

func validateKind(kind Kind) error {
	if kind != KindMarkdown && kind != KindHTML {
		return common.NewError(common.CodeInvalidRequest, "invalid whiteboard kind", nil)
	}
	return nil
}

func resultFrom(record Whiteboard) Result {
	return Result{
		ID:        record.ID,
		Kind:      record.Kind,
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
