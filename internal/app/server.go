package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/common"
)

type ServerConfig struct {
	App             *App
	Logger          *slog.Logger
	Host            string
	Port            int
	ShutdownTimeout time.Duration
	Closers         []io.Closer
	Listener        net.Listener
}

type Server struct {
	app                *App
	logger             *slog.Logger
	host               string
	port               int
	shutdownTimeout    time.Duration
	closers            []io.Closer
	configuredListener net.Listener
	httpServer         *http.Server
	newShutdownContext func(context.Context, time.Duration) (context.Context, context.CancelFunc)

	lifecycleMu  sync.Mutex
	started      bool
	finished     bool
	shuttingDown bool
	closed       bool
	listener     net.Listener

	closeOnce sync.Once
	closeErr  error
}

func NewServer(config ServerConfig) (*Server, error) {
	if config.App == nil || config.App.handler == nil || config.App.readiness == nil {
		return nil, errors.New("server app is required")
	}
	if config.Logger == nil {
		return nil, errors.New("server logger is required")
	}
	if !ValidServerHost(config.Host) {
		return nil, fmt.Errorf("invalid server host %q", config.Host)
	}
	if config.Port < 0 || config.Port > 65535 {
		return nil, fmt.Errorf("invalid server port %d", config.Port)
	}
	if config.ShutdownTimeout <= 0 {
		return nil, errors.New("server shutdown timeout must be positive")
	}
	for _, closer := range config.Closers {
		if common.IsNil(closer) {
			return nil, errors.New("server closer is required")
		}
	}
	if config.Listener != nil {
		if err := ValidateListener(config.Listener); err != nil {
			return nil, fmt.Errorf("invalid configured listener: %w", err)
		}
	}

	return &Server{
		app:                config.App,
		logger:             config.Logger,
		host:               config.Host,
		port:               config.Port,
		shutdownTimeout:    config.ShutdownTimeout,
		closers:            append([]io.Closer(nil), config.Closers...),
		configuredListener: config.Listener,
		httpServer:         &http.Server{Handler: config.App.Handler()},
		newShutdownContext: context.WithTimeout,
		listener:           config.Listener,
	}, nil
}

func (s *Server) Handler() http.Handler {
	return s.app.Handler()
}

func (s *Server) dependenciesReady(ctx context.Context) error {
	return s.app.readiness.dependenciesReady(ctx)
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	if err := s.reserveServe(); err != nil {
		return err
	}

	listener := s.configuredListener
	if listener == nil {
		var err error
		listener, err = net.Listen("tcp", net.JoinHostPort(s.host, strconv.Itoa(s.port)))
		if err != nil {
			return errors.Join(err, s.Close())
		}
	}
	s.setListener(listener)
	return s.serveClaimed(ctx, listener)
}

func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	if err := ValidateListener(listener); err != nil {
		return err
	}
	if err := s.reserveServe(); err != nil {
		return err
	}
	s.setListener(listener)
	return s.serveClaimed(ctx, listener)
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.markShuttingDown()
	return normalizeHTTPServerError(s.httpServer.Shutdown(ctx))
}

func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.lifecycleMu.Lock()
		s.app.SetReady(false)
		s.closed = true
		s.lifecycleMu.Unlock()

		errorsByCloser := make([]error, 0, len(s.closers))
		for _, closer := range s.closers {
			errorsByCloser = append(errorsByCloser, closer.Close())
		}
		s.closeErr = errors.Join(errorsByCloser...)
	})
	return s.closeErr
}

func (s *Server) Address() net.Addr {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

func (s *Server) reserveServe() error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	switch {
	case s.started:
		return errors.New("server has already been served")
	case s.shuttingDown:
		return errors.New("server is shutting down")
	case s.closed:
		return errors.New("server is closed")
	default:
		s.started = true
		return nil
	}
}

func (s *Server) setListener(listener net.Listener) {
	s.lifecycleMu.Lock()
	s.listener = listener
	s.lifecycleMu.Unlock()
}

func (s *Server) serveClaimed(ctx context.Context, listener net.Listener) error {
	serveDone := make(chan error, 1)
	go func() {
		serveErr := normalizeHTTPServerError(s.httpServer.Serve(listener))
		s.finishServing()
		serveDone <- serveErr
	}()

	address := listener.Addr().String()
	logCtx, cancelLog := context.WithCancel(ctx)
	defer cancelLog()
	logDone := make(chan error, 1)
	// Logging runs independently so cancellation and an early Serve exit cannot
	// be hidden by a slow handler. Injected handlers that block should honor ctx.
	// A handler that blocks forever and ignores ctx can retain this goroutine,
	// although lifecycle progress no longer waits for it.
	go func() {
		logDone <- s.logListening(logCtx, address)
	}()

	select {
	case <-ctx.Done():
		cancelLog()
		return s.shutdownAfterCancellation(serveDone, nil)
	case serveErr := <-serveDone:
		cancelLog()
		if ctx.Err() != nil {
			return s.shutdownAfterCancellation(nil, serveErr)
		}
		if serveErr != nil {
			return errors.Join(serveErr, s.Close())
		}
		return nil
	case logErr := <-logDone:
		if logErr != nil {
			return errors.Join(logErr, s.shutdownAfterCancellation(serveDone, nil))
		}
	}

	if ctx.Err() != nil {
		return s.shutdownAfterCancellation(serveDone, nil)
	}
	s.markReady()

	select {
	case <-ctx.Done():
		return s.shutdownAfterCancellation(serveDone, nil)
	case serveErr := <-serveDone:
		if ctx.Err() != nil {
			return s.shutdownAfterCancellation(nil, serveErr)
		}
		if serveErr != nil {
			return errors.Join(serveErr, s.Close())
		}
		return nil
	}
}

func (s *Server) logListening(ctx context.Context, address string) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("server listening log handler panicked")
		}
	}()
	s.logger.InfoContext(ctx, "server listening", "address", address, "url", "http://"+address)
	return nil
}

func (s *Server) shutdownAfterCancellation(serveDone <-chan error, knownServeErr error) error {
	s.markShuttingDown()
	shutdownCtx, cancelShutdown := s.newShutdownContext(context.Background(), s.shutdownTimeout)
	shutdownErr := normalizeHTTPServerError(s.httpServer.Shutdown(shutdownCtx))
	var forceCloseErr error
	if errors.Is(shutdownCtx.Err(), context.DeadlineExceeded) {
		forceCloseErr = normalizeHTTPServerError(s.httpServer.Close())
	}
	cancelShutdown()
	closeErr := s.Close()

	serveErr := knownServeErr
	if serveDone != nil {
		serveErr = <-serveDone
	}
	return errors.Join(shutdownErr, forceCloseErr, closeErr, normalizeHTTPServerError(serveErr))
}

func (s *Server) markShuttingDown() {
	s.lifecycleMu.Lock()
	s.app.SetReady(false)
	s.shuttingDown = true
	s.lifecycleMu.Unlock()
}

func (s *Server) markReady() bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.finished || s.shuttingDown || s.closed {
		return false
	}
	s.app.SetReady(true)
	return true
}

func (s *Server) finishServing() {
	s.lifecycleMu.Lock()
	s.app.SetReady(false)
	s.finished = true
	s.lifecycleMu.Unlock()
}

func normalizeHTTPServerError(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// ValidateListener reports whether listener and its address can be used by a Server.
func ValidateListener(listener net.Listener) error {
	if common.IsNil(listener) {
		return errors.New("listener is required")
	}
	if common.IsNil(listener.Addr()) {
		return errors.New("listener address is required")
	}
	return nil
}

// ValidServerHost reports whether host is a supported IP address or hostname.
func ValidServerHost(host string) bool {
	if host == "" || strings.TrimSpace(host) != host || strings.ContainsAny(host, "[]/\\") {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	if strings.Contains(host, ":") || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') &&
				(character < 'A' || character > 'Z') &&
				(character < '0' || character > '9') &&
				character != '-' {
				return false
			}
		}
	}
	return true
}
