package localapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/agentattachment"
	"github.com/edocsss/agent-whiteboard/internal/agentprotocol"
)

const (
	CommandsPath            = agentprotocol.Namespace + "/commands"
	firstFrameTimeout       = 5 * time.Second
	transportCleanupTimeout = time.Second
)

var errTransportCloseIncomplete = errors.New("local API transport close did not complete")

// TrustSource returns a fresh immutable snapshot of canonical trusted origins.
// Implementations must not return a cached admission decision.
type TrustSource interface {
	TrustedOrigins(context.Context) (map[string]struct{}, error)
}

// Backend is the provider-neutral boundary consumed by the local transport.
type Backend interface {
	Connect(context.Context, string, agentprotocol.Command) (Connection, error)
}

// ImageBackend is the private staged-image boundary used only by the loopback
// HTTP resource. Implementations must enforce the supplied identity again.
type ImageBackend interface {
	Stage(context.Context, agentattachment.StageRequest) (agentattachment.Staged, error)
	Read(context.Context, agentattachment.ReadRequest) (agentattachment.Image, error)
	DeleteStaged(context.Context, agentattachment.DeleteRequest) error
}

// Connection is one admitted browser attachment. Events begins with the
// authoritative snapshot or requested replay. Command and Close must return
// promptly when their context is canceled, and Close must be safe to retry after
// an error. localapi invokes both synchronously so a noncooperative backend
// cannot strand a localapi-owned goroutine.
type Connection interface {
	ConversationID() string
	Events() <-chan agentprotocol.Event
	Command(context.Context, agentprotocol.Command) (agentprotocol.Event, error)
	Close(context.Context) error
}

// RequestRecord is the complete privacy-safe request diagnostic surface.
// Route is a canonical local API route or "unknown"; Code is empty for
// responses without a browser error.
type RequestRecord struct {
	Route  string
	Method string
	Status int
	Code   agentprotocol.BrowserErrorCode
}

// RequestRecorder optionally receives privacy-safe request outcomes.
type RequestRecorder interface {
	Record(RequestRecord)
}

type Config struct {
	Port        int
	TrustSource TrustSource
	Backend     Backend
	Images      ImageBackend
	Recorder    RequestRecorder
}

type Server struct {
	listener net.Listener
	http     *http.Server
	host     string
	trust    TrustSource
	backend  Backend
	images   ImageBackend
	recorder RequestRecorder

	attachments attachmentRegistry
	mu          sync.Mutex
	stopping    bool
	transports  map[*transport]struct{}
	serveErr    chan error
	serveOnce   sync.Once
	closeMu     sync.Mutex
}

// Listen creates the exact IPv4 loopback listener. Port zero selects an
// ephemeral port; no other bind address is supported.
func Listen(config Config) (*Server, error) {
	if config.Port < 0 || config.Port > 65535 {
		return nil, errors.New("local API port must be between 0 and 65535")
	}
	if nilInterface(config.TrustSource) || nilInterface(config.Backend) {
		return nil, errors.New("local API requires trust source and backend")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(config.Port))
	if err != nil {
		return nil, fmt.Errorf("listen local API: %w", err)
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || address.IP == nil || !address.IP.IsLoopback() || address.IP.To4() == nil || address.IP.String() != "127.0.0.1" {
		_ = listener.Close()
		return nil, errors.New("local API listener is not literal IPv4 loopback")
	}

	server := &Server{
		listener:   listener,
		host:       net.JoinHostPort("127.0.0.1", strconv.Itoa(address.Port)),
		trust:      config.TrustSource,
		backend:    config.Backend,
		images:     config.Images,
		recorder:   config.Recorder,
		transports: make(map[*transport]struct{}),
		serveErr:   make(chan error, 1),
	}
	server.http = &http.Server{
		Handler:           server,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    32 << 10,
		IdleTimeout:       75 * time.Second,
		WriteTimeout:      0,
	}
	return server, nil
}

func (s *Server) Addr() net.Addr { return s.listener.Addr() }
func (s *Server) Host() string   { return s.host }

// Serve starts serving and returns immediately. ServeError reports the terminal
// serve result; a normal Close reports nil.
func (s *Server) Serve() {
	s.serveOnce.Do(func() {
		go func() {
			err := s.http.Serve(s.listener)
			if isClosedError(err) {
				err = nil
			}
			s.serveErr <- err
			close(s.serveErr)
		}()
	})
}

func (s *Server) ServeError() <-chan error { return s.serveErr }

func (s *Server) Close(ctx context.Context) error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()

	var errs []error
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	if err := s.listener.Close(); err != nil && !isClosedError(err) {
		errs = append(errs, err)
	}
	if err := ctx.Err(); err != nil {
		errs = append(errs, errTransportCloseIncomplete, err)
		return errors.Join(errs...)
	}

	s.mu.Lock()
	transports := make([]*transport, 0, len(s.transports))
	for item := range s.transports {
		transports = append(transports, item)
	}
	s.mu.Unlock()
	for _, item := range transports {
		if err := item.close(ctx); err != nil {
			errs = append(errs, err)
			if ctx.Err() != nil {
				break
			}
			continue
		}
		s.untrack(item)
	}
	if err := s.http.Shutdown(ctx); err != nil && !isClosedError(err) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

type closeState struct {
	mu      sync.Mutex
	running bool
	closed  bool
	done    chan struct{}
}

func (s *closeState) run(ctx context.Context, fn func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return errors.Join(errTransportCloseIncomplete, err)
	}
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil
		}
		if s.running {
			done := s.done
			s.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return errors.Join(errTransportCloseIncomplete, ctx.Err())
			}
		}
		s.running = true
		s.done = make(chan struct{})
		done := s.done
		s.mu.Unlock()

		err := fn(ctx)

		s.mu.Lock()
		s.running = false
		if err == nil {
			s.closed = true
		}
		close(done)
		s.mu.Unlock()
		return err
	}
}

type transport struct {
	state closeState
	fn    func(context.Context) error
}

func (t *transport) close(ctx context.Context) error {
	return t.state.run(ctx, t.fn)
}

func (s *Server) track(fn func(context.Context) error) *transport {
	item := &transport{fn: fn}
	s.mu.Lock()
	stopping := s.stopping
	if !stopping {
		s.transports[item] = struct{}{}
	}
	s.mu.Unlock()
	if stopping {
		ctx, cancel := context.WithTimeout(context.Background(), transportCleanupTimeout)
		defer cancel()
		_ = item.close(ctx)
	}
	return item
}

func (s *Server) isStopping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopping
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func isClosedError(err error) bool {
	return errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed)
}

func (s *Server) untrack(item *transport) {
	s.mu.Lock()
	delete(s.transports, item)
	s.mu.Unlock()
}
