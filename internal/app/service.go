package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"

	"github.com/edocsss/agent-whiteboard/internal/image"
	"github.com/edocsss/agent-whiteboard/internal/store"
	"github.com/edocsss/agent-whiteboard/internal/whiteboard"
)

type Service struct {
	whiteboards *whiteboard.Service
	images      *image.Service
	server      *Server
}

func NewService(config ServiceConfig, options ...Option) (*Service, error) {
	resolved, err := resolveServiceConfig(config, options)
	if err != nil {
		return nil, err
	}

	whiteboardStore := resolved.whiteboardStore
	imageStore := resolved.imageStore
	closers := make([]io.Closer, 0, 2)
	var ownedFilesystem *filesystemLifecycle
	if whiteboardStore == nil || imageStore == nil {
		lifetimeCtx, cancelLifetime := context.WithCancel(context.Background())
		filesystem, fsErr := store.NewFS(store.Config{
			Root:            resolved.rootDir,
			CleanupInterval: resolved.cleanupInterval,
			Clock:           resolved.clock,
			Context:         lifetimeCtx,
		})
		if fsErr != nil {
			cancelLifetime()
			return nil, fsErr
		}
		ownedFilesystem = &filesystemLifecycle{cancel: cancelLifetime, filesystem: filesystem}
		closers = append(closers, ownedFilesystem)
		if whiteboardStore == nil {
			whiteboardStore = filesystem.Whiteboards()
		}
		if imageStore == nil {
			imageStore = filesystem.Images()
		}
	}

	if resolved.whiteboardStore != nil {
		closers = append(closers, resolved.whiteboardStore)
	}
	if resolved.imageStore != nil {
		closers = append(closers, resolved.imageStore)
	}

	fail := func(constructionErr error) (*Service, error) {
		if ownedFilesystem == nil {
			return nil, constructionErr
		}
		return nil, errors.Join(constructionErr, ownedFilesystem.Close())
	}

	whiteboardService, err := whiteboard.NewService(whiteboardStore, resolved.clock, resolved.ids, resolved.defaultExpiration)
	if err != nil {
		return fail(err)
	}
	imageService, err := image.NewService(imageStore, resolved.clock, resolved.ids, resolved.defaultExpiration, resolved.logger)
	if err != nil {
		return fail(err)
	}
	viewer, err := whiteboard.NewViewer(whiteboard.ViewerConfig{CSS: resolved.viewerCSS, JS: resolved.viewerJS})
	if err != nil {
		return fail(err)
	}
	whiteboardHandler, err := whiteboard.NewHandler(whiteboardService, viewer, whiteboard.HandlerConfig{
		MaxWhiteboardBytes: resolved.maxWhiteboardBytes,
		MaxContextBytes:    resolved.maxContextBytes,
	})
	if err != nil {
		return fail(err)
	}
	imageHandler, err := image.NewHandler(imageService, image.HandlerConfig{
		MaxImageBytes:   resolved.maxImageBytes,
		MaxRequestBytes: resolved.maxImageRequestBytes,
	})
	if err != nil {
		return fail(err)
	}
	application, err := New(Config{
		Whiteboards: whiteboardHandler,
		Images:      imageHandler,
		Readiness:   []Readiness{whiteboardStore, imageStore},
	})
	if err != nil {
		return fail(err)
	}
	server, err := NewServer(ServerConfig{
		App:             application,
		Logger:          resolved.logger,
		Host:            resolved.host,
		Port:            resolved.port,
		ShutdownTimeout: resolved.shutdownTimeout,
		Closers:         closers,
		Listener:        resolved.listener,
	})
	if err != nil {
		return fail(err)
	}

	return &Service{whiteboards: whiteboardService, images: imageService, server: server}, nil
}

func (service *Service) CreateMarkdown(ctx context.Context, input whiteboard.CreateInput) (whiteboard.Result, error) {
	return service.whiteboards.CreateMarkdown(ctx, input)
}

func (service *Service) CreateHTML(ctx context.Context, input whiteboard.CreateInput) (whiteboard.Result, error) {
	return service.whiteboards.CreateHTML(ctx, input)
}

func (service *Service) GetWhiteboard(ctx context.Context, id string) (whiteboard.Whiteboard, error) {
	return service.whiteboards.Get(ctx, id)
}

func (service *Service) UpdateWhiteboard(ctx context.Context, input whiteboard.UpdateInput) (whiteboard.Result, error) {
	return service.whiteboards.Update(ctx, input)
}

func (service *Service) DeleteWhiteboard(ctx context.Context, kind whiteboard.Kind, id string) error {
	return service.whiteboards.Delete(ctx, kind, id)
}

func (service *Service) CreateImages(ctx context.Context, input image.CreateInput) ([]image.Result, error) {
	return service.images.CreateImages(ctx, input)
}

func (service *Service) GetImage(ctx context.Context, id string) (image.Image, error) {
	return service.images.Get(ctx, id)
}

func (service *Service) UpdateImage(ctx context.Context, input image.UpdateInput) (image.Result, error) {
	return service.images.Update(ctx, input)
}

func (service *Service) DeleteImage(ctx context.Context, id string) error {
	return service.images.Delete(ctx, id)
}

func (service *Service) Handler() http.Handler {
	return service.server.Handler()
}

func (service *Service) ListenAndServe(ctx context.Context) error {
	return service.server.ListenAndServe(ctx)
}

func (service *Service) Serve(ctx context.Context, listener net.Listener) error {
	return service.server.Serve(ctx, listener)
}

func (service *Service) Shutdown(ctx context.Context) error {
	return service.server.Shutdown(ctx)
}

func (service *Service) Close() error {
	return service.server.Close()
}

type filesystemLifecycle struct {
	cancel     context.CancelFunc
	filesystem *store.FS
}

func (lifecycle *filesystemLifecycle) Close() error {
	lifecycle.cancel()
	return lifecycle.filesystem.Close()
}
