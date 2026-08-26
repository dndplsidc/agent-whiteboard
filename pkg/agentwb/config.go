package agentwb

import (
	"net"

	"github.com/dndplsidc/agent-whiteboard/internal/app"
)

type Config = app.ServiceConfig
type Option = app.Option
type LogMode = app.LogMode

const (
	LogModeConsole = app.LogModeConsole
	LogModeJSON    = app.LogModeJSON
)

func WithPort(port int) Option                   { return app.WithPort(port) }
func WithDefaultExpiration(seconds int64) Option { return app.WithDefaultExpiration(seconds) }
func WithClock(clock Clock) Option               { return app.WithClock(clock) }
func WithIDGenerator(ids IDGenerator) Option     { return app.WithIDGenerator(ids) }
func WithListener(listener net.Listener) Option  { return app.WithListener(listener) }
func WithViewerAssets(css, js []byte) Option     { return app.WithViewerAssets(css, js) }
