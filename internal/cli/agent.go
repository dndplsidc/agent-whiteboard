package cli

import (
	"errors"
	"os"

	"github.com/edocsss/agent-whiteboard/internal/config"
	"github.com/spf13/cobra"
)

const handlesConfigurationAnnotation = "agent-whiteboard/handles-configuration"

func (factory commandFactory) newAgentCommand() *cobra.Command {
	command := &cobra.Command{
		Use:  "agent",
		Args: usageArgs(cobra.NoArgs),
	}
	command.AddCommand(factory.newAgentServeCommand(), factory.newAgentDaemonCommand(), factory.newTrustCommand())
	return command
}

func (factory commandFactory) newTrustCommand() *cobra.Command {
	command := &cobra.Command{
		Use:  "trust",
		Args: usageArgs(cobra.NoArgs),
	}
	command.AddCommand(
		factory.newTrustMutationCommand("add", config.AddTrustedOrigin),
		factory.newTrustMutationCommand("remove", config.RemoveTrustedOrigin),
		factory.newTrustListCommand(),
	)
	return command
}

func (factory commandFactory) newTrustMutationCommand(name string, edit func(string, string) error) *cobra.Command {
	return &cobra.Command{
		Use:         name + " ORIGIN",
		Args:        usageArgs(cobra.ExactArgs(1)),
		Annotations: map[string]string{handlesConfigurationAnnotation: "true"},
		RunE: func(_ *cobra.Command, args []string) error {
			canonical, err := config.CanonicalOrigin(args[0])
			if err != nil {
				return invalidCommand(err.Error())
			}
			if err := edit(factory.root.configPath, canonical); err != nil {
				return trustConfigurationError(err)
			}
			return writeDeleteSuccess(factory.deps.Stdout, factory.root.json)
		},
	}
}

func (factory commandFactory) newTrustListCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "list",
		Args:        usageArgs(cobra.NoArgs),
		Annotations: map[string]string{handlesConfigurationAnnotation: "true"},
		RunE: func(_ *cobra.Command, _ []string) error {
			origins, err := config.ListTrustedOrigins(factory.root.configPath)
			if err != nil {
				return trustConfigurationError(err)
			}
			return writeTrustedOrigins(factory.deps.Stdout, factory.root.json, origins)
		},
	}
}

func trustConfigurationError(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return invalidCommand("configuration file does not exist")
	}
	return invalidCommand(err.Error())
}
