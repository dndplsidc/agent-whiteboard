package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edocsss/agent-whiteboard/internal/app"
	"github.com/edocsss/agent-whiteboard/internal/common"
	generalconfig "github.com/edocsss/agent-whiteboard/internal/config"
	httpx "github.com/edocsss/agent-whiteboard/internal/webapi"
	"github.com/spf13/cobra"
)

type Client interface {
	CreateWhiteboard(context.Context, httpx.WhiteboardKind, httpx.File, *int64) (httpx.Resource, error)
	CreateMarkdown(context.Context, httpx.File, httpx.File, *int64) (httpx.Resource, error)
	UpdateWhiteboard(context.Context, httpx.WhiteboardKind, string, httpx.File, *int64) (httpx.Resource, error)
	UpdateMarkdown(context.Context, string, httpx.File, httpx.File, *int64) (httpx.Resource, error)
	GetMarkdown(context.Context, string) (httpx.MarkdownResponse, error)
	DeleteWhiteboard(context.Context, httpx.WhiteboardKind, string) error
	CreateImages(context.Context, []httpx.File, *int64) ([]httpx.Resource, error)
	UpdateImage(context.Context, string, httpx.File, *int64) (httpx.Resource, error)
	DeleteImage(context.Context, string) error
	PublicURL(string) (string, error)
}

type Application interface {
	ListenAndServe(context.Context) error
	Close() error
}

type Dependencies struct {
	Stdout                io.Writer
	Stderr                io.Writer
	Getenv                func(string) string
	LoadConfig            func(string) (generalconfig.Config, error)
	NewClient             func(httpx.ClientConfig) (Client, error)
	NewApplication        func(app.ServiceConfig, ...app.Option) (Application, error)
	NewAgentApplication   func(app.AgentServiceConfig) (Application, error)
	NewLaunchAgentManager func() (common.LaunchAgentManager, error)
	ExecutablePath        func() (string, error)
}

type rootOptions struct {
	server     string
	timeout    string
	configPath string
	json       bool
}

type clientSettings struct {
	server  string
	timeout time.Duration
}

type serverFlagValues struct {
	host, port, storage, cleanupInterval, defaultExpiration string
	shutdownTimeout, logMode                                string
	maxWhiteboardBytes, maxContextBytes                     string
	maxImageBytes, maxImageRequestBytes                     string
}

type resolvedServerSettings struct {
	host                 string
	port                 int
	storage              string
	cleanupInterval      time.Duration
	defaultExpiration    int64
	shutdownTimeout      time.Duration
	logMode              string
	maxWhiteboardBytes   int64
	maxContextBytes      int64
	maxImageBytes        int64
	maxImageRequestBytes int64
	localAgentEnabled    bool
}

type generalConfiguration struct {
	loaded   generalconfig.Config
	builtins generalconfig.BuiltinValues
}

type commandFactory struct {
	deps    Dependencies
	root    *rootOptions
	general *generalConfiguration
}

func NewRoot(deps Dependencies) (*cobra.Command, error) {
	if common.IsNil(deps.Stdout) {
		return nil, invalidCommand("stdout is required")
	}
	if common.IsNil(deps.Stderr) {
		return nil, invalidCommand("stderr is required")
	}
	if common.IsNil(deps.Getenv) {
		return nil, invalidCommand("environment lookup is required")
	}
	if common.IsNil(deps.NewClient) {
		return nil, invalidCommand("client factory is required")
	}
	if common.IsNil(deps.NewApplication) {
		return nil, invalidCommand("application factory is required")
	}
	if deps.NewAgentApplication == nil {
		deps.NewAgentApplication = func(config app.AgentServiceConfig) (Application, error) {
			return app.NewAgentService(config)
		}
	}
	if common.IsNil(deps.NewLaunchAgentManager) {
		deps.NewLaunchAgentManager = func() (common.LaunchAgentManager, error) {
			return common.NewLaunchAgentManager(common.LaunchAgentExecRunner{})
		}
	}
	if common.IsNil(deps.ExecutablePath) {
		deps.ExecutablePath = os.Executable
	}

	options := &rootOptions{}
	root := &cobra.Command{
		Use:           "agent-whiteboard",
		Short:         "Publish artifacts for trusted agents",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.SetOut(deps.Stdout)
	root.SetErr(deps.Stderr)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return markUsage(err) })
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentFlags().StringVar(&options.server, "server", "", "server origin")
	root.PersistentFlags().StringVar(&options.timeout, "timeout", "", "client timeout")
	root.PersistentFlags().StringVar(&options.configPath, "config", "", "configuration file")
	root.PersistentFlags().BoolVar(&options.json, "json", false, "write versioned JSON output")

	if deps.LoadConfig == nil {
		deps.LoadConfig = generalconfig.Load
	}
	factory := commandFactory{deps: deps, root: options, general: &generalConfiguration{}}
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if cmd == root || isCompletionRequest(cmd) || commandHandlesConfiguration(cmd) || isDaemonServeRequest(cmd) {
			return nil
		}
		return factory.loadGeneralConfiguration()
	}
	root.AddCommand(factory.newServeCommand(), factory.newCreateCommand(), factory.newUpdateCommand(), factory.newGetCommand(), factory.newDeleteCommand(), factory.newImageCommand(), factory.newAgentCommand())
	return root, nil
}

func (factory commandFactory) loadGeneralConfiguration() error {
	loaded, err := factory.deps.LoadConfig(factory.root.configPath)
	if err != nil {
		return invalidCommand(err.Error())
	}
	builtins, err := generalconfig.Builtins()
	if err != nil {
		return invalidCommand(err.Error())
	}
	factory.general.loaded = loaded
	factory.general.builtins = builtins
	return nil
}

func isCompletionRequest(cmd *cobra.Command) bool {
	calledAs := cmd.CalledAs()
	return calledAs == cobra.ShellCompRequestCmd || calledAs == cobra.ShellCompNoDescRequestCmd
}

func commandHandlesConfiguration(cmd *cobra.Command) bool {
	return cmd.Annotations[handlesConfigurationAnnotation] == "true"
}

func isDaemonServeRequest(cmd *cobra.Command) bool {
	if cmd == nil || cmd.Name() != "serve" || cmd.Parent() == nil || cmd.Parent().Name() != "agent" {
		return false
	}
	flag := cmd.Flags().Lookup("daemon")
	return flag != nil && flag.Value.String() == "true"
}

// selectedProviderExecutableResolver records explicit provider selections without
// copying any ambient environment into the LaunchAgent. Absolute selections
// are returned as-is; names are resolved using the current PATH.
type selectedProviderExecutableResolver struct{ pi, codex string }

func (resolver selectedProviderExecutableResolver) LookPath(name string) (string, error) {
	selected := ""
	switch name {
	case common.LaunchAgentProviderPi:
		selected = resolver.pi
	case common.LaunchAgentProviderCodex:
		selected = resolver.codex
	default:
		return "", exec.ErrNotFound
	}
	if selected == "" {
		return exec.LookPath(name)
	}
	if filepath.IsAbs(selected) {
		return selected, nil
	}
	path, err := exec.LookPath(selected)
	if err != nil {
		// An absent provider discovered through the ordinary PATH is optional,
		// but a non-empty flag or environment selection is an explicit request
		// and must make daemon installation fail.
		return "", fmt.Errorf("explicit %s executable %q is unavailable: %v", name, selected, err)
	}
	return path, nil
}

func (factory commandFactory) newClient(cmd *cobra.Command) (Client, context.Context, context.CancelFunc, error) {
	settings, err := factory.resolveClientSettings(cmd)
	if err != nil {
		return nil, nil, nil, err
	}
	client, err := factory.deps.NewClient(httpx.ClientConfig{
		Server: settings.server,
		HTTPClient: &http.Client{
			Timeout: settings.timeout,
		},
	})
	if err != nil {
		return nil, nil, nil, stableCommandError(err)
	}
	if common.IsNil(client) {
		return nil, nil, nil, errors.New("client factory returned nil")
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), settings.timeout)
	return client, ctx, cancel, nil
}

func (factory commandFactory) resolveClientSettings(cmd *cobra.Command) (clientSettings, error) {
	server := factory.general.builtins.Client.Server
	if value, set := factory.general.loaded.Client().Server(); set {
		server = value
	}
	if value := factory.deps.Getenv("AGENT_WHITEBOARD_SERVER"); value != "" {
		server = value
	}
	if cmd.Flags().Changed("server") {
		server = factory.root.server
	}
	if err := validateServerOrigin(server); err != nil {
		return clientSettings{}, err
	}

	timeoutText := factory.general.builtins.Client.Timeout.String()
	if value, set := factory.general.loaded.Client().Timeout(); set {
		timeoutText = value.String()
	}
	if value := factory.deps.Getenv("AGENT_WHITEBOARD_TIMEOUT"); value != "" {
		timeoutText = value
	}
	if cmd.Flags().Changed("timeout") {
		timeoutText = factory.root.timeout
	}
	timeout, err := time.ParseDuration(timeoutText)
	if err != nil || timeout <= 0 {
		return clientSettings{}, invalidCommand("timeout must be a positive duration")
	}
	return clientSettings{server: server, timeout: timeout}, nil
}

func validateServerOrigin(value string) error {
	if strings.Contains(value, "#") {
		return invalidCommand("server must be an absolute HTTP origin")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return invalidCommand("server must be an absolute HTTP origin")
	}
	if parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return invalidCommand("server must be an absolute HTTP origin")
	}
	if (parsed.Path != "" && parsed.Path != "/") || (parsed.RawPath != "" && parsed.RawPath != "/") {
		return invalidCommand("server must be an absolute HTTP origin")
	}
	return nil
}

func (factory commandFactory) resolveServerSettings(cmd *cobra.Command, flags *serverFlagValues) (resolvedServerSettings, error) {
	builtins := factory.general.builtins.Server
	configured := factory.general.loaded.Server()
	yamlString := func(value string, set bool, fallback string) string {
		if set {
			return value
		}
		return fallback
	}
	get := func(flagName, envName, flagValue, yamlValue string) string {
		if cmd.Flags().Changed(flagName) {
			return flagValue
		}
		if value := factory.deps.Getenv(envName); value != "" {
			return value
		}
		return yamlValue
	}

	host, hostSet := configured.Host()
	storage, storageSet := configured.Storage()
	logMode, logModeSet := configured.LogMode()
	port, portSet := configured.Port()
	cleanup, cleanupSet := configured.CleanupInterval()
	expires, expiresSet := configured.DefaultExpiresIn()
	shutdown, shutdownSet := configured.ShutdownTimeout()
	maxWhiteboard, maxWhiteboardSet := configured.MaxWhiteboardBytes()
	maxContext, maxContextSet := configured.MaxContextBytes()
	maxImage, maxImageSet := configured.MaxImageBytes()
	maxImageRequest, maxImageRequestSet := configured.MaxImageRequestBytes()

	settings := resolvedServerSettings{
		host:    get("host", "AGENT_WHITEBOARD_HOST", flags.host, yamlString(host, hostSet, builtins.Host)),
		storage: get("storage", "AGENT_WHITEBOARD_STORAGE", flags.storage, yamlString(storage, storageSet, builtins.Storage)),
		logMode: get("log-mode", "AGENT_WHITEBOARD_LOG_MODE", flags.logMode, yamlString(logMode, logModeSet, builtins.LogMode)),
	}
	settings.localAgentEnabled = factory.general.builtins.Viewer.LocalAgent.Enabled
	if enabled, set := factory.general.loaded.Viewer().LocalAgent().Enabled(); set {
		settings.localAgentEnabled = enabled
	}

	var err error
	if settings.port, err = parseInt(get("port", "AGENT_WHITEBOARD_PORT", flags.port, yamlString(strconv.Itoa(port), portSet, strconv.Itoa(builtins.Port))), "port"); err != nil {
		return resolvedServerSettings{}, err
	}
	if settings.cleanupInterval, err = parsePositiveDuration(get("cleanup-interval", "AGENT_WHITEBOARD_CLEANUP_INTERVAL", flags.cleanupInterval, yamlString(cleanup.String(), cleanupSet, builtins.CleanupInterval.String())), "cleanup interval"); err != nil {
		return resolvedServerSettings{}, err
	}
	if settings.defaultExpiration, err = parseNonnegativeInt64(get("default-expires-in", "AGENT_WHITEBOARD_DEFAULT_EXPIRES_IN", flags.defaultExpiration, yamlString(strconv.FormatInt(expires, 10), expiresSet, strconv.FormatInt(builtins.DefaultExpiresIn, 10))), "default expiration"); err != nil {
		return resolvedServerSettings{}, err
	}
	if settings.shutdownTimeout, err = parsePositiveDuration(get("shutdown-timeout", "AGENT_WHITEBOARD_SHUTDOWN_TIMEOUT", flags.shutdownTimeout, yamlString(shutdown.String(), shutdownSet, builtins.ShutdownTimeout.String())), "shutdown timeout"); err != nil {
		return resolvedServerSettings{}, err
	}
	if settings.maxWhiteboardBytes, err = parseNonnegativeInt64(get("max-whiteboard-bytes", "AGENT_WHITEBOARD_MAX_WHITEBOARD_BYTES", flags.maxWhiteboardBytes, yamlString(strconv.FormatInt(maxWhiteboard, 10), maxWhiteboardSet, strconv.FormatInt(builtins.MaxWhiteboardBytes, 10))), "max whiteboard bytes"); err != nil {
		return resolvedServerSettings{}, err
	}
	if settings.maxContextBytes, err = parseNonnegativeInt64(get("max-context-bytes", "AGENT_WHITEBOARD_MAX_CONTEXT_BYTES", flags.maxContextBytes, yamlString(strconv.FormatInt(maxContext, 10), maxContextSet, strconv.FormatInt(builtins.MaxContextBytes, 10))), "max context bytes"); err != nil {
		return resolvedServerSettings{}, err
	}
	if settings.maxImageBytes, err = parseNonnegativeInt64(get("max-image-bytes", "AGENT_WHITEBOARD_MAX_IMAGE_BYTES", flags.maxImageBytes, yamlString(strconv.FormatInt(maxImage, 10), maxImageSet, strconv.FormatInt(builtins.MaxImageBytes, 10))), "max image bytes"); err != nil {
		return resolvedServerSettings{}, err
	}
	if settings.maxImageRequestBytes, err = parseNonnegativeInt64(get("max-image-request-bytes", "AGENT_WHITEBOARD_MAX_IMAGE_REQUEST_BYTES", flags.maxImageRequestBytes, yamlString(strconv.FormatInt(maxImageRequest, 10), maxImageRequestSet, strconv.FormatInt(builtins.MaxImageRequestBytes, 10))), "max image request bytes"); err != nil {
		return resolvedServerSettings{}, err
	}

	switch {
	case !app.ValidServerHost(settings.host):
		return resolvedServerSettings{}, invalidCommand("invalid server host")
	case settings.port < 0 || settings.port > 65535:
		return resolvedServerSettings{}, invalidCommand("port must be between 0 and 65535")
	case strings.TrimSpace(settings.storage) == "":
		return resolvedServerSettings{}, invalidCommand("storage path is required")
	case settings.logMode != "console" && settings.logMode != "json":
		return resolvedServerSettings{}, invalidCommand("log mode must be console or json")
	case effectiveLimit(settings.maxImageRequestBytes, builtins.MaxImageRequestBytes) < effectiveLimit(settings.maxImageBytes, builtins.MaxImageBytes):
		return resolvedServerSettings{}, invalidCommand("max image request bytes must not be less than max image bytes")
	}
	return settings, nil
}

func effectiveLimit(value, defaultValue int64) int64 {
	if value == 0 {
		return defaultValue
	}
	return value
}

func parseInt(value, field string) (int, error) {
	parsed, err := strconv.ParseInt(value, 10, 0)
	if err != nil {
		return 0, invalidCommand(field + " must be an integer")
	}
	return int(parsed), nil
}

func parseNonnegativeInt64(value, field string) (int64, error) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, invalidCommand(field + " must be an integer")
	}
	if parsed < 0 {
		return 0, invalidCommand(field + " must not be negative")
	}
	return parsed, nil
}

func parsePositiveDuration(value, field string) (time.Duration, error) {
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, invalidCommand(field + " must be a positive duration")
	}
	return parsed, nil
}

func invalidCommand(message string) error {
	return markUsage(common.NewError(common.CodeInvalidRequest, message, nil))
}

type usageError struct{ err error }

func (err usageError) Error() string { return err.err.Error() }
func (err usageError) Unwrap() error { return err.err }

func markUsage(err error) error {
	if err == nil {
		return nil
	}
	var marked usageError
	if errors.As(err, &marked) {
		return err
	}
	return usageError{err: err}
}

func usageArgs(validation cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		return markUsage(validation(cmd, args))
	}
}

func stableCommandError(err error) error {
	if err == nil {
		return nil
	}
	contextErr, contextOnly := contextOnlyError(err)
	if !contextOnly {
		return err
	}
	if contextErr == context.DeadlineExceeded {
		return stableContextError{message: "request timed out", cause: context.DeadlineExceeded}
	}
	return stableContextError{message: "request canceled", cause: context.Canceled}
}

func contextOnlyError(err error) (error, bool) {
	if err == nil {
		return nil, false
	}
	if err == context.DeadlineExceeded {
		return context.DeadlineExceeded, true
	}
	if err == context.Canceled {
		return context.Canceled, true
	}
	type multiUnwrapper interface{ Unwrap() []error }
	if multi, ok := err.(multiUnwrapper); ok {
		children := multi.Unwrap()
		if len(children) == 0 {
			return nil, false
		}
		kind := error(context.Canceled)
		for _, child := range children {
			childKind, childOnly := contextOnlyError(child)
			if !childOnly {
				return nil, false
			}
			if childKind == context.DeadlineExceeded {
				kind = context.DeadlineExceeded
			}
		}
		return kind, true
	}
	unwrapped := errors.Unwrap(err)
	if unwrapped == nil {
		return nil, false
	}
	return contextOnlyError(unwrapped)
}

type stableContextError struct {
	message string
	cause   error
}

func (err stableContextError) Error() string { return err.message }
func (err stableContextError) Unwrap() error { return err.cause }

func expirationFlag(command *cobra.Command) *string {
	value := new(string)
	command.Flags().StringVar(value, "expires-in", "", "expiration in seconds; zero means permanent")
	return value
}

func resolveExpiration(command *cobra.Command, value string) (*int64, error) {
	if !command.Flags().Changed("expires-in") {
		return nil, nil
	}
	parsed, err := parseNonnegativeInt64(value, "expiration")
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func openRegularFile(path string) (*os.File, httpx.File, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, httpx.File{}, invalidCommand(fmt.Sprintf("cannot open file %q", path))
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, httpx.File{}, invalidCommand(fmt.Sprintf("file %q must be a regular file", path))
	}
	return file, httpx.File{Name: filepath.Base(path), Reader: file}, nil
}
