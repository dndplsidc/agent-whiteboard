package app

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"

	"github.com/dndplsidc/agent-whiteboard/internal/common"
	"github.com/dndplsidc/agent-whiteboard/internal/image"
	httpx "github.com/dndplsidc/agent-whiteboard/internal/webapi"
	"github.com/dndplsidc/agent-whiteboard/internal/whiteboard"
)

type Readiness interface {
	Ready(context.Context) error
}

type Config struct {
	Whiteboards *whiteboard.Handler
	Images      *image.Handler
	Readiness   []Readiness
}

type App struct {
	handler   http.Handler
	readiness *readiness
}

type readiness struct {
	accepting    atomic.Bool
	dependencies []Readiness
}

func New(config Config) (*App, error) {
	switch {
	case config.Whiteboards == nil:
		return nil, common.NewError(common.CodeInvalidRequest, "whiteboard handler is required", nil)
	case config.Images == nil:
		return nil, common.NewError(common.CodeInvalidRequest, "image handler is required", nil)
	}
	for _, dependency := range config.Readiness {
		if common.IsNil(dependency) {
			return nil, common.NewError(common.CodeInvalidRequest, "readiness dependency is required", nil)
		}
	}

	state := &readiness{dependencies: append([]Readiness(nil), config.Readiness...)}
	mux := http.NewServeMux()
	config.Whiteboards.Register(mux)
	config.Images.Register(mux)
	httpx.RegisterHealth(mux, state)

	return &App{handler: mux, readiness: state}, nil
}

func (a *App) Handler() http.Handler {
	return a.handler
}

func (a *App) SetReady(ready bool) {
	a.readiness.accepting.Store(ready)
}

func (r *readiness) Ready(ctx context.Context) error {
	if !r.accepting.Load() {
		return errors.New("not accepting requests")
	}
	return r.dependenciesReady(ctx)
}

func (r *readiness) dependenciesReady(ctx context.Context) error {
	for _, dependency := range r.dependencies {
		if err := dependency.Ready(ctx); err != nil {
			return err
		}
	}
	return nil
}
