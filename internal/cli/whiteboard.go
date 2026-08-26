package cli

import (
	"os"

	"github.com/dndplsidc/agent-whiteboard/internal/webapi"
	"github.com/spf13/cobra"
)

func (factory commandFactory) newCreateCommand() *cobra.Command {
	command := &cobra.Command{Use: "create", Args: usageArgs(cobra.NoArgs)}
	command.AddCommand(factory.newCreateWhiteboardCommand("markdown", webapi.WhiteboardMarkdown))
	command.AddCommand(factory.newCreateWhiteboardCommand("html", webapi.WhiteboardHTML))
	return command
}

func (factory commandFactory) newCreateWhiteboardCommand(name string, kind webapi.WhiteboardKind) *cobra.Command {
	command := &cobra.Command{Use: name + " <file>", Args: usageArgs(cobra.ExactArgs(1))}
	expires := expirationFlag(command)
	contextPath := contextFlag(command)
	command.RunE = func(cmd *cobra.Command, args []string) error {
		if err := requireContextFlag(cmd, *contextPath); err != nil {
			return err
		}
		expiration, err := resolveExpiration(cmd, *expires)
		if err != nil {
			return err
		}
		openedSource, source, openedContext, creatorContext, err := openWhiteboardPair(args[0], *contextPath)
		if err != nil {
			return err
		}
		defer openedSource.Close()
		defer openedContext.Close()

		client, ctx, cancel, err := factory.newClient(cmd)
		if err != nil {
			return err
		}
		defer cancel()
		var created webapi.Resource
		if kind == webapi.WhiteboardMarkdown {
			created, err = client.CreateMarkdown(ctx, source, creatorContext, expiration)
		} else {
			created, err = client.CreateHTML(ctx, source, creatorContext, expiration)
		}
		return factory.finishCreate(client, created, err)
	}
	return command
}

func (factory commandFactory) newUpdateCommand() *cobra.Command {
	command := &cobra.Command{Use: "update", Args: usageArgs(cobra.NoArgs)}
	command.AddCommand(factory.newUpdateWhiteboardCommand("markdown", webapi.WhiteboardMarkdown))
	command.AddCommand(factory.newUpdateWhiteboardCommand("html", webapi.WhiteboardHTML))
	return command
}

func (factory commandFactory) newUpdateWhiteboardCommand(name string, kind webapi.WhiteboardKind) *cobra.Command {
	command := &cobra.Command{Use: name + " <id> <file>", Args: usageArgs(cobra.ExactArgs(2))}
	expires := expirationFlag(command)
	contextPath := contextFlag(command)
	command.RunE = func(cmd *cobra.Command, args []string) error {
		if err := requireContextFlag(cmd, *contextPath); err != nil {
			return err
		}
		expiration, err := resolveExpiration(cmd, *expires)
		if err != nil {
			return err
		}
		openedSource, source, openedContext, creatorContext, err := openWhiteboardPair(args[1], *contextPath)
		if err != nil {
			return err
		}
		defer openedSource.Close()
		defer openedContext.Close()

		client, ctx, cancel, err := factory.newClient(cmd)
		if err != nil {
			return err
		}
		defer cancel()
		var updated webapi.Resource
		if kind == webapi.WhiteboardMarkdown {
			updated, err = client.UpdateMarkdown(ctx, args[0], source, creatorContext, expiration)
		} else {
			updated, err = client.UpdateHTML(ctx, args[0], source, creatorContext, expiration)
		}
		if err != nil {
			return stableCommandError(err)
		}
		return stableCommandError(writeResource(factory.deps.Stdout, factory.root.json, client, updated))
	}
	return command
}

func (factory commandFactory) newGetCommand() *cobra.Command {
	command := &cobra.Command{Use: "get", Args: usageArgs(cobra.NoArgs)}
	command.AddCommand(factory.newGetWhiteboardCommand("markdown", webapi.WhiteboardMarkdown))
	command.AddCommand(factory.newGetWhiteboardCommand("html", webapi.WhiteboardHTML))
	return command
}

func (factory commandFactory) newGetWhiteboardCommand(name string, kind webapi.WhiteboardKind) *cobra.Command {
	return &cobra.Command{
		Use:  name + " <id>",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !factory.root.json {
				return invalidCommand("get " + name + " requires --json")
			}
			client, ctx, cancel, err := factory.newClient(cmd)
			if err != nil {
				return err
			}
			defer cancel()
			if kind == webapi.WhiteboardMarkdown {
				response, err := client.GetMarkdown(ctx, args[0])
				if err != nil {
					return stableCommandError(err)
				}
				return stableCommandError(writeMarkdown(factory.deps.Stdout, client, response))
			}
			response, err := client.GetHTML(ctx, args[0])
			if err != nil {
				return stableCommandError(err)
			}
			return stableCommandError(writeHTML(factory.deps.Stdout, client, response))
		},
	}
}

func (factory commandFactory) newDeleteCommand() *cobra.Command {
	command := &cobra.Command{Use: "delete", Args: usageArgs(cobra.NoArgs)}
	command.AddCommand(factory.newDeleteWhiteboardCommand("markdown", webapi.WhiteboardMarkdown))
	command.AddCommand(factory.newDeleteWhiteboardCommand("html", webapi.WhiteboardHTML))
	return command
}

func (factory commandFactory) newDeleteWhiteboardCommand(name string, kind webapi.WhiteboardKind) *cobra.Command {
	return &cobra.Command{
		Use:  name + " <id>",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, ctx, cancel, err := factory.newClient(cmd)
			if err != nil {
				return err
			}
			defer cancel()
			if err := client.DeleteWhiteboard(ctx, kind, args[0]); err != nil {
				return stableCommandError(err)
			}
			return writeDeleteSuccess(factory.deps.Stdout, factory.root.json)
		},
	}
}

func (factory commandFactory) finishCreate(client Client, created webapi.Resource, createErr error) error {
	if createErr == nil || created.ID != "" {
		if err := writeResource(factory.deps.Stdout, factory.root.json, client, created); err != nil {
			return stableCommandError(err)
		}
	}
	return stableCommandError(createErr)
}

func contextFlag(command *cobra.Command) *string {
	value := new(string)
	command.Flags().StringVar(value, "context", "", "creator context Markdown file")
	return value
}

func requireContextFlag(command *cobra.Command, path string) error {
	if !command.Flags().Changed("context") || path == "" {
		return invalidCommand("context file is required")
	}
	return nil
}

func openWhiteboardPair(sourcePath, contextPath string) (*os.File, webapi.File, *os.File, webapi.File, error) {
	openedSource, source, err := openRegularFile(sourcePath)
	if err != nil {
		return nil, webapi.File{}, nil, webapi.File{}, err
	}
	openedContext, creatorContext, err := openRegularFile(contextPath)
	if err != nil {
		_ = openedSource.Close()
		return nil, webapi.File{}, nil, webapi.File{}, err
	}
	return openedSource, source, openedContext, creatorContext, nil
}
