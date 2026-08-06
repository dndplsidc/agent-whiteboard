package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/edocsss/agent-whiteboard/internal/common"
	generalconfig "github.com/edocsss/agent-whiteboard/internal/config"
	"github.com/edocsss/agent-whiteboard/internal/launchagent"
	"github.com/spf13/cobra"
)

const daemonConfigurationIndependentAnnotation = handlesConfigurationAnnotation

type piProviderDescriptor struct{}

func (piProviderDescriptor) ProviderName() string   { return launchagent.ProviderPi }
func (piProviderDescriptor) ExecutableName() string { return launchagent.ProviderPi }

type codexProviderDescriptor struct{}

func (codexProviderDescriptor) ProviderName() string   { return launchagent.ProviderCodex }
func (codexProviderDescriptor) ExecutableName() string { return launchagent.ProviderCodex }

func (factory commandFactory) newAgentDaemonCommand() *cobra.Command {
	command := &cobra.Command{
		Use:         "daemon",
		Args:        usageArgs(cobra.NoArgs),
		Annotations: map[string]string{daemonConfigurationIndependentAnnotation: "true"},
	}
	command.AddCommand(
		factory.newAgentDaemonStatusCommand(),
		factory.newAgentDaemonMutationCommand("restart", func(ctx context.Context, manager launchagent.Manager) error {
			return manager.Restart(ctx)
		}),
		factory.newAgentDaemonMutationCommand("stop", func(ctx context.Context, manager launchagent.Manager) error {
			return manager.Stop(ctx)
		}),
		factory.newAgentDaemonMutationCommand("uninstall", func(ctx context.Context, manager launchagent.Manager) error {
			return manager.Uninstall(ctx)
		}),
	)
	return command
}

func (factory commandFactory) newAgentDaemonStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "status",
		Args:        usageArgs(cobra.NoArgs),
		Annotations: map[string]string{daemonConfigurationIndependentAnnotation: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			manager, err := factory.newLaunchAgentManager()
			if err != nil {
				return err
			}
			status, err := manager.Status(cmd.Context())
			if err != nil {
				return err
			}
			return writeDaemonStatus(factory.deps.Stdout, factory.root.json, status)
		},
	}
}

func (factory commandFactory) newAgentDaemonMutationCommand(name string, operation func(context.Context, launchagent.Manager) error) *cobra.Command {
	return &cobra.Command{
		Use:         name,
		Args:        usageArgs(cobra.NoArgs),
		Annotations: map[string]string{daemonConfigurationIndependentAnnotation: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			manager, err := factory.newLaunchAgentManager()
			if err != nil {
				return err
			}
			if err := operation(cmd.Context(), manager); err != nil {
				return err
			}
			return writeDeleteSuccess(factory.deps.Stdout, factory.root.json)
		},
	}
}

func (factory commandFactory) newLaunchAgentManager() (launchagent.Manager, error) {
	manager, err := factory.deps.NewLaunchAgentManager()
	if err != nil {
		return nil, err
	}
	if common.IsNil(manager) {
		return nil, errors.New("launch agent manager factory returned nil")
	}
	return manager, nil
}

func rejectDaemonFlags(cmd *cobra.Command) error {
	for _, name := range []string{"port", "provider-idle-timeout", "shutdown-timeout"} {
		if cmd.Flags().Changed(name) {
			return invalidCommand(fmt.Sprintf("--%s cannot be used with --daemon", name))
		}
	}
	return nil
}

func (factory commandFactory) installAgentDaemon(cmd *cobra.Command, piFlagValue string, piFlagSet bool, codexFlagValue string, codexFlagSet bool) error {
	manager, err := factory.newLaunchAgentManager()
	if err != nil {
		return err
	}
	executable, err := factory.resolveExecutablePath()
	if err != nil {
		return err
	}
	configuration, err := factory.resolveDaemonConfigPath()
	if err != nil {
		return err
	}
	piSelected := piFlagValue
	if !piFlagSet {
		piSelected = factory.deps.Getenv(launchagent.PiExecutableEnvironment)
	}
	codexSelected := codexFlagValue
	if !codexFlagSet {
		codexSelected = factory.deps.Getenv(launchagent.CodexExecutableEnvironment)
	}
	install := launchagent.Config{
		Executable:         executable,
		ConfigPath:         configuration,
		Providers:          []launchagent.ProviderDescriptor{piProviderDescriptor{}, codexProviderDescriptor{}},
		ExecutableResolver: nil,
	}
	if piSelected != "" || codexSelected != "" {
		install.ExecutableResolver = selectedProviderExecutableResolver{pi: piSelected, codex: codexSelected}
	}
	return manager.Install(cmd.Context(), install)
}

func (factory commandFactory) resolveExecutablePath() (string, error) {
	path, err := factory.deps.ExecutablePath()
	if err != nil {
		return "", fmt.Errorf("resolve agent-whiteboard executable: %w", err)
	}
	if path == "" {
		return "", errors.New("resolve agent-whiteboard executable: empty path")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve agent-whiteboard executable path: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func (factory commandFactory) resolveDaemonConfigPath() (string, error) {
	if factory.root.configPath == "" {
		return generalconfig.DefaultPath()
	}
	return generalconfig.ResolvePath(factory.root.configPath)
}
