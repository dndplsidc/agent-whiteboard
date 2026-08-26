package agentwb

import "github.com/dndplsidc/agent-whiteboard/internal/app"

func New(config Config, options ...Option) (*Service, error) {
	return app.NewService(config, options...)
}
