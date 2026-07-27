package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/edocsss/agent-whiteboard/internal/agentstate"
	"github.com/edocsss/agent-whiteboard/internal/broker"
	"github.com/edocsss/agent-whiteboard/internal/common"
	generalconfig "github.com/edocsss/agent-whiteboard/internal/config"
	"github.com/edocsss/agent-whiteboard/internal/localapi"
	"github.com/edocsss/agent-whiteboard/internal/pi"
	"github.com/edocsss/agent-whiteboard/internal/processgroup"
	"github.com/edocsss/agent-whiteboard/internal/provider"
)

// AgentServiceConfig describes the foreground agent composition. Defaults for
// port and timeout values are selected by the CLI from configuration builtins;
// this package intentionally does not select those defaults.
type AgentServiceConfig struct {
	ConfigPath string
	Home       string

	Port                int
	PiExecutable        string
	ProviderEnvironment []string
	IdleTimeout         time.Duration
	ShutdownTimeout     time.Duration

	Launcher provider.Launcher
	IDs      common.IDGenerator
	Clock    common.Clock
	Timers   broker.TimerFactory
}

// AgentService owns the agent state, provider broker, and local API listener.
type AgentService struct {
	state           *agentstate.Store
	broker          *broker.Broker
	local           *localapi.Server
	shutdownTimeout time.Duration

	closeMu sync.Mutex
}

// NewAgentService composes the foreground agent service and cleans up every
// dependency that was successfully opened when a later dependency fails.
func NewAgentService(config AgentServiceConfig) (*AgentService, error) {
	if config.Port < 0 || config.Port > 65535 {
		return nil, errors.New("agent port must be between 0 and 65535")
	}
	if config.IdleTimeout <= 0 {
		return nil, errors.New("agent provider idle timeout must be positive")
	}
	if config.ShutdownTimeout <= 0 {
		return nil, errors.New("agent shutdown timeout must be positive")
	}

	launcher := config.Launcher
	if common.IsNil(launcher) {
		launcher = processgroup.NewLauncher()
	}
	ids := config.IDs
	if common.IsNil(ids) {
		ids = common.CryptoIDGenerator{}
	}
	clock := config.Clock
	if common.IsNil(clock) {
		clock = common.SystemClock{}
	}
	timers := config.Timers
	if common.IsNil(timers) {
		timers = broker.RealTimerFactory{}
	}

	executable, err := resolvePiExecutable(config.PiExecutable)
	if err != nil {
		return nil, err
	}
	environment, err := providerEnvironment(config.Home, config.ProviderEnvironment)
	if err != nil {
		return nil, err
	}

	state, err := agentstate.Open(config.Home)
	if err != nil {
		return nil, fmt.Errorf("open agent state: %w", err)
	}
	cleanupState := func(constructionErr error) (*AgentService, error) {
		return nil, errors.Join(constructionErr, state.Close())
	}
	providerRoot, err := state.EnsureProviderDirectory(provider.NamePi)
	if err != nil {
		return cleanupState(fmt.Errorf("ensure Pi provider directory: %w", err))
	}
	driver, err := pi.NewDriver(pi.Config{
		Executable: executable, Environment: environment, ProviderRoot: providerRoot,
		Launcher: launcher, IDs: ids, Clock: clock,
	})
	if err != nil {
		return cleanupState(fmt.Errorf("create Pi driver: %w", err))
	}
	backend, err := broker.New(broker.Config{
		State: state, Driver: driver, IDs: ids, Clock: clock, Timers: timers,
		IdleTimeout: config.IdleTimeout, ShutdownTimeout: config.ShutdownTimeout,
	})
	if err != nil {
		return cleanupState(fmt.Errorf("create agent broker: %w", err))
	}
	cleanupBroker := func(constructionErr error) (*AgentService, error) {
		closeCtx, cancel := context.WithTimeout(context.Background(), config.ShutdownTimeout)
		defer cancel()
		return nil, errors.Join(constructionErr, backend.Close(closeCtx), state.Close())
	}
	local, err := localapi.Listen(localapi.Config{
		Port: config.Port, TrustSource: configTrustSource{selectedPath: config.ConfigPath}, Backend: backend,
	})
	if err != nil {
		return cleanupBroker(fmt.Errorf("create local API: %w", err))
	}
	return &AgentService{state: state, broker: backend, local: local, shutdownTimeout: config.ShutdownTimeout}, nil
}

// Addr exposes the bound local API address for in-process callers and tests.
func (service *AgentService) Addr() net.Addr {
	if service == nil || service.local == nil {
		return nil
	}
	return service.local.Addr()
}

// Host exposes the bound local API host and port for in-process callers and tests.
func (service *AgentService) Host() string {
	if service == nil || service.local == nil {
		return ""
	}
	return service.local.Host()
}

// ListenAndServe starts the local API and waits for either its terminal serve
// result or context cancellation. Shutdown is always attempted in ownership
// order before returning.
func (service *AgentService) ListenAndServe(ctx context.Context) error {
	if service == nil || service.local == nil {
		return errors.New("agent service is not initialized")
	}
	service.local.Serve()
	var serveErr error
	select {
	case serveErr = <-service.local.ServeError():
	case <-ctx.Done():
	}
	closeErr := service.Close()
	if ctx.Err() != nil && serveErr == nil {
		return closeErr
	}
	return errors.Join(serveErr, closeErr)
}

// Close retries each owned component with an independent bounded context. The
// component close implementations are retry-safe, so a timed-out close can be
// retried by a later caller.
func (service *AgentService) Close() error {
	if service == nil {
		return nil
	}
	service.closeMu.Lock()
	defer service.closeMu.Unlock()

	var errs []error
	if service.local != nil {
		ctx, cancel := context.WithTimeout(context.Background(), service.shutdownTimeout)
		errs = append(errs, service.local.Close(ctx))
		cancel()
	}
	brokerClosed := service.broker == nil
	if service.broker != nil {
		ctx, cancel := context.WithTimeout(context.Background(), service.shutdownTimeout)
		brokerErr := service.broker.Close(ctx)
		cancel()
		errs = append(errs, brokerErr)
		brokerClosed = brokerErr == nil
	}
	if brokerClosed && service.state != nil {
		errs = append(errs, service.state.Close())
	}
	return errors.Join(errs...)
}

type configTrustSource struct{ selectedPath string }

func (source configTrustSource) TrustedOrigins(ctx context.Context) (map[string]struct{}, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	loaded, err := generalconfig.Load(source.selectedPath)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	origins, _ := loaded.Agent().TrustedOrigins()
	trusted := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		trusted[origin] = struct{}{}
	}
	return trusted, nil
}

func resolvePiExecutable(value string) (string, error) {
	resolved := value
	if resolved == "" {
		var err error
		resolved, err = exec.LookPath("pi")
		if err != nil {
			return "", fmt.Errorf("resolve Pi executable: %w", err)
		}
	} else if !filepath.IsAbs(resolved) {
		var err error
		resolved, err = exec.LookPath(resolved)
		if err != nil {
			return "", fmt.Errorf("resolve Pi executable: %w", err)
		}
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve Pi executable path: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func providerEnvironment(home string, override []string) ([]string, error) {
	if override != nil {
		cloned := make([]string, len(override))
		copy(cloned, override)
		if err := validateEnvironment(cloned); err != nil {
			return nil, err
		}
		return cloned, nil
	}
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve provider home: %w", err)
		}
	}
	absoluteHome, err := filepath.Abs(home)
	if err != nil {
		return nil, fmt.Errorf("resolve provider home: %w", err)
	}
	absoluteHome = filepath.Clean(absoluteHome)
	environment := []string{
		"HOME=" + absoluteHome,
		"USERPROFILE=" + absoluteHome,
		"PATH=" + os.Getenv("PATH"),
	}
	for _, name := range []string{"TMPDIR", "LANG", "LC_ALL"} {
		if value := os.Getenv(name); value != "" {
			environment = append(environment, name+"="+value)
		}
	}
	environment = append(environment,
		"PI_OFFLINE=1",
		"PI_SKIP_VERSION_CHECK=1",
		"PI_TELEMETRY=0",
	)
	if err := validateEnvironment(environment); err != nil {
		return nil, err
	}
	return environment, nil
}

func validateEnvironment(environment []string) error {
	seen := make(map[string]struct{}, len(environment))
	for _, entry := range environment {
		if !utf8.ValidString(entry) {
			return errors.New("provider environment contains invalid UTF-8")
		}
		if strings.ContainsRune(entry, '\x00') {
			return errors.New("provider environment contains NUL")
		}
		separator := strings.IndexByte(entry, '=')
		if separator <= 0 {
			return errors.New("provider environment entry must contain a name")
		}
		name := entry[:separator]
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("duplicate provider environment variable %q", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

var _ interface {
	ListenAndServe(context.Context) error
	Close() error
} = (*AgentService)(nil)
