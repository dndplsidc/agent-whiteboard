package cli

import (
	"errors"
	"io"
	"log/slog"

	"github.com/dndplsidc/agent-whiteboard/internal/app"
	"github.com/dndplsidc/agent-whiteboard/internal/common"
	"github.com/spf13/cobra"
)

type applicationArguments struct {
	config            app.ServiceConfig
	port              int
	defaultExpiration int64
}

func buildApplicationArguments(settings resolvedServerSettings, logWriter io.Writer) applicationArguments {
	var handler slog.Handler
	if settings.logMode == string(app.LogModeJSON) {
		handler = slog.NewJSONHandler(logWriter, nil)
	} else {
		handler = slog.NewTextHandler(logWriter, nil)
	}
	return applicationArguments{
		config: app.ServiceConfig{
			RootDir:                 settings.storage,
			CleanupInterval:         settings.cleanupInterval,
			Host:                    settings.host,
			ShutdownTimeout:         settings.shutdownTimeout,
			MaxWhiteboardBytes:      settings.maxWhiteboardBytes,
			MaxContextBytes:         settings.maxContextBytes,
			MaxImageBytes:           settings.maxImageBytes,
			MaxImageRequestBytes:    settings.maxImageRequestBytes,
			ViewerLocalAgentEnabled: settings.localAgentEnabled,
			LogMode:                 app.LogMode(settings.logMode),
			Logger:                  slog.New(handler),
		},
		port:              settings.port,
		defaultExpiration: settings.defaultExpiration,
	}
}

func (arguments applicationArguments) options() []app.Option {
	return []app.Option{
		app.WithPort(arguments.port),
		app.WithDefaultExpiration(arguments.defaultExpiration),
	}
}

func (factory commandFactory) newServeCommand() *cobra.Command {
	values := &serverFlagValues{}
	command := &cobra.Command{
		Use:  "serve",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return factory.runServe(cmd, values)
		},
	}
	flags := command.Flags()
	flags.StringVar(&values.host, "host", "", "bind host")
	flags.StringVar(&values.port, "port", "", "bind port")
	flags.StringVar(&values.storage, "storage", "", "storage root")
	flags.StringVar(&values.cleanupInterval, "cleanup-interval", "", "cleanup interval")
	flags.StringVar(&values.defaultExpiration, "default-expires-in", "", "default expiration in seconds")
	flags.StringVar(&values.shutdownTimeout, "shutdown-timeout", "", "shutdown timeout")
	flags.StringVar(&values.logMode, "log-mode", "", "console or json logging")
	flags.StringVar(&values.maxWhiteboardBytes, "max-whiteboard-bytes", "", "maximum whiteboard size")
	flags.StringVar(&values.maxContextBytes, "max-context-bytes", "", "maximum Markdown context size")
	flags.StringVar(&values.maxImageBytes, "max-image-bytes", "", "maximum image size")
	flags.StringVar(&values.maxImageRequestBytes, "max-image-request-bytes", "", "maximum image request size")
	return command
}

func (factory commandFactory) runServe(cmd *cobra.Command, values *serverFlagValues) (resultErr error) {
	settings, err := factory.resolveServerSettings(cmd, values)
	if err != nil {
		return err
	}
	arguments := buildApplicationArguments(settings, factory.deps.Stderr)
	application, err := factory.deps.NewApplication(arguments.config, arguments.options()...)
	if err != nil {
		return err
	}
	if common.IsNil(application) {
		return errors.New("application factory returned nil")
	}
	defer func() {
		resultErr = errors.Join(resultErr, application.Close())
	}()

	resultErr = application.ListenAndServe(cmd.Context())
	if contextErr := cmd.Context().Err(); contextErr != nil {
		resultContext, contextOnly := contextOnlyError(resultErr)
		if resultErr == nil || (contextOnly && resultContext == contextErr) {
			resultErr = nil
		}
	}
	return resultErr
}
