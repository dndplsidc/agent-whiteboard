package app

import (
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/assets"
	"github.com/edocsss/agent-whiteboard/internal/common"
	generalconfig "github.com/edocsss/agent-whiteboard/internal/config"
	"github.com/edocsss/agent-whiteboard/internal/image"
	"github.com/edocsss/agent-whiteboard/internal/whiteboard"
)

const (
	defaultExpirationSeconds    int64 = 86400
	defaultCleanupInterval            = 15 * time.Minute
	defaultHost                       = "127.0.0.1"
	defaultPort                       = 8567
	defaultShutdownTimeout            = 10 * time.Second
	defaultMaxWhiteboardBytes   int64 = 10 << 20
	defaultMaxContextBytes      int64 = generalconfig.DefaultMaxContextBytes
	defaultMaxImageBytes        int64 = 25 << 20
	defaultMaxImageRequestBytes int64 = 100 << 20
)

var userHomeDir = os.UserHomeDir

type LogMode string

const (
	LogModeConsole LogMode = "console"
	LogModeJSON    LogMode = "json"
)

type ServiceConfig struct {
	WhiteboardStore whiteboard.Store
	ImageStore      image.Store

	RootDir                  string
	DefaultExpirationSeconds int64
	CleanupInterval          time.Duration

	Host            string
	Port            int
	ShutdownTimeout time.Duration

	MaxWhiteboardBytes   int64
	MaxContextBytes      int64
	MaxImageBytes        int64
	MaxImageRequestBytes int64

	ViewerLocalAgentEnabled bool

	LogMode LogMode
	Logger  *slog.Logger
}

type Option func(*optionValues) error

type optionValues struct {
	port                 int
	portSet              bool
	defaultExpiration    int64
	defaultExpirationSet bool
	clock                common.Clock
	clockSet             bool
	ids                  common.IDGenerator
	idsSet               bool
	listener             net.Listener
	listenerSet          bool
	viewerCSS            []byte
	viewerJS             []byte
	viewerAssetsSet      bool
}

type resolvedServiceConfig struct {
	whiteboardStore whiteboard.Store
	imageStore      image.Store
	rootDir         string

	defaultExpiration int64
	cleanupInterval   time.Duration
	clock             common.Clock
	ids               common.IDGenerator

	host            string
	port            int
	shutdownTimeout time.Duration
	listener        net.Listener

	maxWhiteboardBytes   int64
	maxContextBytes      int64
	maxImageBytes        int64
	maxImageRequestBytes int64

	viewerLocalAgentEnabled bool

	logMode LogMode
	logger  *slog.Logger

	viewerCSS []byte
	viewerJS  []byte
}

func WithPort(port int) Option {
	return func(values *optionValues) error {
		values.port, values.portSet = port, true
		return nil
	}
}

func WithDefaultExpiration(seconds int64) Option {
	return func(values *optionValues) error {
		values.defaultExpiration, values.defaultExpirationSet = seconds, true
		return nil
	}
}

func WithClock(clock common.Clock) Option {
	return func(values *optionValues) error {
		values.clock, values.clockSet = clock, true
		return nil
	}
}

func WithIDGenerator(ids common.IDGenerator) Option {
	return func(values *optionValues) error {
		values.ids, values.idsSet = ids, true
		return nil
	}
}

func WithListener(listener net.Listener) Option {
	return func(values *optionValues) error {
		values.listener, values.listenerSet = listener, true
		return nil
	}
}

func WithViewerAssets(css, js []byte) Option {
	cssCopy := bytes.Clone(css)
	jsCopy := bytes.Clone(js)
	return func(values *optionValues) error {
		values.viewerCSS = bytes.Clone(cssCopy)
		values.viewerJS = bytes.Clone(jsCopy)
		values.viewerAssetsSet = true
		return nil
	}
}

func resolveServiceConfig(config ServiceConfig, options []Option) (resolvedServiceConfig, error) {
	if common.IsNil(config.WhiteboardStore) && config.WhiteboardStore != nil {
		return resolvedServiceConfig{}, invalidServiceConfig("whiteboard store is required")
	}
	if common.IsNil(config.ImageStore) && config.ImageStore != nil {
		return resolvedServiceConfig{}, invalidServiceConfig("image store is required")
	}

	values := optionValues{}
	for _, option := range options {
		if option == nil {
			return resolvedServiceConfig{}, invalidServiceConfig("option is required")
		}
		if err := option(&values); err != nil {
			return resolvedServiceConfig{}, err
		}
	}

	rootDir := config.RootDir
	if rootDir == "" && (config.WhiteboardStore == nil || config.ImageStore == nil) {
		home, err := userHomeDir()
		if err != nil {
			return resolvedServiceConfig{}, fmt.Errorf("resolve home directory: %w", err)
		}
		rootDir = filepath.Join(home, ".agent-whiteboard")
	}
	defaultExpiration := config.DefaultExpirationSeconds
	if defaultExpiration == 0 {
		defaultExpiration = defaultExpirationSeconds
	}
	if values.defaultExpirationSet {
		defaultExpiration = values.defaultExpiration
	}
	cleanupInterval := config.CleanupInterval
	if cleanupInterval == 0 {
		cleanupInterval = defaultCleanupInterval
	}
	host := config.Host
	if host == "" {
		host = defaultHost
	}
	port := config.Port
	if port == 0 {
		port = defaultPort
	}
	if values.portSet {
		port = values.port
	}
	shutdownTimeout := config.ShutdownTimeout
	if shutdownTimeout == 0 {
		shutdownTimeout = defaultShutdownTimeout
	}
	maxWhiteboardBytes := config.MaxWhiteboardBytes
	if maxWhiteboardBytes == 0 {
		maxWhiteboardBytes = defaultMaxWhiteboardBytes
	}
	maxContextBytes := config.MaxContextBytes
	if maxContextBytes == 0 {
		maxContextBytes = defaultMaxContextBytes
	}
	maxImageBytes := config.MaxImageBytes
	if maxImageBytes == 0 {
		maxImageBytes = defaultMaxImageBytes
	}
	maxImageRequestBytes := config.MaxImageRequestBytes
	if maxImageRequestBytes == 0 {
		maxImageRequestBytes = defaultMaxImageRequestBytes
	}
	logMode := config.LogMode
	if logMode == "" {
		logMode = LogModeConsole
	}

	clock := common.Clock(common.SystemClock{})
	if values.clockSet {
		clock = values.clock
	}
	ids := common.IDGenerator(common.CryptoIDGenerator{})
	if values.idsSet {
		ids = values.ids
	}
	viewerCSS := assets.ViewerCSS()
	viewerJS := assets.ViewerJS()
	if values.viewerAssetsSet {
		viewerCSS = bytes.Clone(values.viewerCSS)
		viewerJS = bytes.Clone(values.viewerJS)
	}

	if err := validateResolvedServiceConfig(config, values, defaultExpiration, cleanupInterval, host, port, shutdownTimeout, maxWhiteboardBytes, maxContextBytes, maxImageBytes, maxImageRequestBytes, logMode, clock, ids, viewerCSS, viewerJS); err != nil {
		return resolvedServiceConfig{}, err
	}

	logger := config.Logger
	if logger == nil {
		var handler slog.Handler
		if logMode == LogModeJSON {
			handler = slog.NewJSONHandler(os.Stderr, nil)
		} else {
			handler = slog.NewTextHandler(os.Stderr, nil)
		}
		logger = slog.New(handler)
	}

	return resolvedServiceConfig{
		whiteboardStore:         config.WhiteboardStore,
		imageStore:              config.ImageStore,
		rootDir:                 rootDir,
		defaultExpiration:       defaultExpiration,
		cleanupInterval:         cleanupInterval,
		clock:                   clock,
		ids:                     ids,
		host:                    host,
		port:                    port,
		shutdownTimeout:         shutdownTimeout,
		listener:                values.listener,
		maxWhiteboardBytes:      maxWhiteboardBytes,
		maxContextBytes:         maxContextBytes,
		maxImageBytes:           maxImageBytes,
		maxImageRequestBytes:    maxImageRequestBytes,
		viewerLocalAgentEnabled: config.ViewerLocalAgentEnabled,
		logMode:                 logMode,
		logger:                  logger,
		viewerCSS:               bytes.Clone(viewerCSS),
		viewerJS:                bytes.Clone(viewerJS),
	}, nil
}

func validateResolvedServiceConfig(
	config ServiceConfig,
	values optionValues,
	defaultExpiration int64,
	cleanupInterval time.Duration,
	host string,
	port int,
	shutdownTimeout time.Duration,
	maxWhiteboardBytes int64,
	maxContextBytes int64,
	maxImageBytes int64,
	maxImageRequestBytes int64,
	logMode LogMode,
	clock common.Clock,
	ids common.IDGenerator,
	viewerCSS []byte,
	viewerJS []byte,
) error {
	if values.listenerSet {
		if err := ValidateListener(values.listener); err != nil {
			return invalidServiceConfig("invalid listener")
		}
	}

	switch {
	case config.DefaultExpirationSeconds < 0 || defaultExpiration < 0:
		return invalidServiceConfig("default expiration must not be negative")
	case config.CleanupInterval < 0 || cleanupInterval <= 0:
		return invalidServiceConfig("cleanup interval must be positive")
	case !ValidServerHost(host):
		return invalidServiceConfig("invalid server host")
	case config.Port < 0 || config.Port > 65535 || port < 0 || port > 65535:
		return invalidServiceConfig("port must be between 0 and 65535")
	case config.ShutdownTimeout < 0 || shutdownTimeout <= 0:
		return invalidServiceConfig("shutdown timeout must be positive")
	case config.MaxWhiteboardBytes < 0 || maxWhiteboardBytes < 0:
		return invalidServiceConfig("max whiteboard bytes must not be negative")
	case config.MaxContextBytes < 0 || maxContextBytes < 0:
		return invalidServiceConfig("max context bytes must not be negative")
	case config.MaxImageBytes < 0 || maxImageBytes < 0:
		return invalidServiceConfig("max image bytes must not be negative")
	case config.MaxImageRequestBytes < 0 || maxImageRequestBytes < 0:
		return invalidServiceConfig("max image request bytes must not be negative")
	case maxImageRequestBytes < maxImageBytes:
		return invalidServiceConfig("max image request bytes must not be less than max image bytes")
	case logMode != LogModeConsole && logMode != LogModeJSON:
		return invalidServiceConfig("invalid log mode")
	case common.IsNil(clock):
		return invalidServiceConfig("clock is required")
	case common.IsNil(ids):
		return invalidServiceConfig("id generator is required")
	case len(viewerCSS) == 0:
		return invalidServiceConfig("viewer CSS is required")
	case len(viewerJS) == 0:
		return invalidServiceConfig("viewer JavaScript is required")
	default:
		return nil
	}
}

func invalidServiceConfig(message string) error {
	return common.NewError(common.CodeInvalidRequest, message, nil)
}
