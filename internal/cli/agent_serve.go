package cli

import (
	"errors"
	"strconv"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/app"
	"github.com/dndplsidc/agent-whiteboard/internal/common"
	"github.com/spf13/cobra"
)

type agentServeFlagValues struct {
	port, providerIdleTimeout, shutdownTimeout, piExecutable, codexExecutable string
	daemon                                                                    bool
}

type resolvedAgentSettings struct {
	port                int
	providerIdleTimeout time.Duration
	shutdownTimeout     time.Duration
	piExecutable        string
	codexExecutable     string
}

func (factory commandFactory) newAgentServeCommand() *cobra.Command {
	values := &agentServeFlagValues{}
	command := &cobra.Command{
		Use:  "serve",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return factory.runAgentServe(cmd, values)
		},
	}
	flags := command.Flags()
	flags.StringVar(&values.port, "port", "", "bind port")
	flags.StringVar(&values.providerIdleTimeout, "provider-idle-timeout", "", "provider idle timeout")
	flags.StringVar(&values.shutdownTimeout, "shutdown-timeout", "", "shutdown timeout")
	flags.StringVar(&values.piExecutable, "pi-executable", "", "Pi executable")
	flags.StringVar(&values.codexExecutable, "codex-executable", "", "Codex executable")
	flags.BoolVar(&values.daemon, "daemon", false, "install and start the managed daemon")
	return command
}

func (factory commandFactory) resolveAgentSettings(cmd *cobra.Command, flags *agentServeFlagValues) (resolvedAgentSettings, error) {
	builtins := factory.general.builtins.Agent
	configured := factory.general.loaded.Agent()
	stringValue := func(flagName, envName, flagValue, yamlValue string, yamlSet bool, builtin string) string {
		if cmd.Flags().Changed(flagName) {
			return flagValue
		}
		if value := factory.deps.Getenv(envName); value != "" {
			return value
		}
		if yamlSet {
			return yamlValue
		}
		return builtin
	}

	port, portSet := configured.Port()
	portText := stringValue("port", "AGENT_WHITEBOARD_AGENT_PORT", flags.port, strconv.Itoa(port), portSet, strconv.Itoa(builtins.Port))
	parsedPort, err := parseInt(portText, "agent port")
	if err != nil {
		return resolvedAgentSettings{}, err
	}
	if parsedPort < 0 || parsedPort > 65535 {
		return resolvedAgentSettings{}, invalidCommand("agent port must be between 0 and 65535")
	}

	idle, idleSet := configured.ProviderIdleTimeout()
	idleText := stringValue("provider-idle-timeout", "AGENT_WHITEBOARD_PROVIDER_IDLE_TIMEOUT", flags.providerIdleTimeout, idle.String(), idleSet, builtins.ProviderIdleTimeout.String())
	providerIdleTimeout, err := parsePositiveDuration(idleText, "provider idle timeout")
	if err != nil {
		return resolvedAgentSettings{}, err
	}

	shutdown, shutdownSet := configured.ShutdownTimeout()
	shutdownText := stringValue("shutdown-timeout", "AGENT_WHITEBOARD_AGENT_SHUTDOWN_TIMEOUT", flags.shutdownTimeout, shutdown.String(), shutdownSet, builtins.ShutdownTimeout.String())
	shutdownTimeout, err := parsePositiveDuration(shutdownText, "agent shutdown timeout")
	if err != nil {
		return resolvedAgentSettings{}, err
	}

	piExecutable := ""
	if cmd.Flags().Changed("pi-executable") {
		piExecutable = flags.piExecutable
	} else {
		piExecutable = factory.deps.Getenv("AGENT_WHITEBOARD_PROVIDER_PI_EXECUTABLE")
	}
	codexExecutable := ""
	if cmd.Flags().Changed("codex-executable") {
		codexExecutable = flags.codexExecutable
	} else {
		codexExecutable = factory.deps.Getenv("AGENT_WHITEBOARD_PROVIDER_CODEX_EXECUTABLE")
	}
	return resolvedAgentSettings{
		port: parsedPort, providerIdleTimeout: providerIdleTimeout,
		shutdownTimeout: shutdownTimeout, piExecutable: piExecutable, codexExecutable: codexExecutable,
	}, nil
}

func (factory commandFactory) runAgentServe(cmd *cobra.Command, flags *agentServeFlagValues) (resultErr error) {
	if flags.daemon {
		if err := rejectDaemonFlags(cmd); err != nil {
			return err
		}
		if cmd.Flags().Changed("pi-executable") && flags.piExecutable == "" {
			return invalidCommand("Pi executable must not be empty")
		}
		if cmd.Flags().Changed("codex-executable") && flags.codexExecutable == "" {
			return invalidCommand("Codex executable must not be empty")
		}
		if err := factory.installAgentDaemon(cmd, flags.piExecutable, cmd.Flags().Changed("pi-executable"), flags.codexExecutable, cmd.Flags().Changed("codex-executable")); err != nil {
			return err
		}
		return writeDeleteSuccess(factory.deps.Stdout, factory.root.json)
	}

	if cmd.Flags().Changed("pi-executable") && flags.piExecutable == "" {
		return invalidCommand("Pi executable must not be empty")
	}
	if cmd.Flags().Changed("codex-executable") && flags.codexExecutable == "" {
		return invalidCommand("Codex executable must not be empty")
	}
	settings, err := factory.resolveAgentSettings(cmd, flags)
	if err != nil {
		return err
	}
	application, err := factory.deps.NewAgentApplication(app.AgentServiceConfig{
		ConfigPath:      factory.root.configPath,
		Port:            settings.port,
		PiExecutable:    settings.piExecutable,
		CodexExecutable: settings.codexExecutable,
		IdleTimeout:     settings.providerIdleTimeout,
		ShutdownTimeout: settings.shutdownTimeout,
	})
	if err != nil {
		return err
	}
	if common.IsNil(application) {
		return errors.New("agent application factory returned nil")
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
