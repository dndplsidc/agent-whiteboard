package cli

import (
	"os"

	"github.com/edocsss/agent-whiteboard/internal/http"
	"github.com/spf13/cobra"
)

func (factory commandFactory) newCreateCommand() *cobra.Command {
	command := &cobra.Command{Use: "create", Args: usageArgs(cobra.NoArgs)}
	command.AddCommand(factory.newCreateMarkdownCommand())
	command.AddCommand(factory.newCreateWhiteboardCommand("html", http.WhiteboardHTML))
	return command
}

func (factory commandFactory) newCreateMarkdownCommand() *cobra.Command {
	command := &cobra.Command{Use: "markdown <file>", Args: usageArgs(cobra.ExactArgs(1))}
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
		openedSource, source, openedContext, creatorContext, err := openMarkdownPair(args[0], *contextPath)
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
		created, err := client.CreateMarkdown(ctx, source, creatorContext, expiration)
		if err != nil {
			return stableCommandError(err)
		}
		return stableCommandError(writeResource(factory.deps.Stdout, factory.root.json, client, created))
	}
	return command
}

func (factory commandFactory) newCreateWhiteboardCommand(name string, kind http.WhiteboardKind) *cobra.Command {
	command := &cobra.Command{Use: name + " <file>", Args: usageArgs(cobra.ExactArgs(1))}
	expires := expirationFlag(command)
	command.RunE = func(cmd *cobra.Command, args []string) error {
		expiration, err := resolveExpiration(cmd, *expires)
		if err != nil {
			return err
		}
		opened, file, err := openRegularFile(args[0])
		if err != nil {
			return err
		}
		defer opened.Close()

		client, ctx, cancel, err := factory.newClient(cmd)
		if err != nil {
			return err
		}
		defer cancel()
		created, err := client.CreateWhiteboard(ctx, kind, file, expiration)
		if err != nil {
			return stableCommandError(err)
		}
		return stableCommandError(writeResource(factory.deps.Stdout, factory.root.json, client, created))
	}
	return command
}

func (factory commandFactory) newUpdateCommand() *cobra.Command {
	command := &cobra.Command{Use: "update", Args: usageArgs(cobra.NoArgs)}
	command.AddCommand(factory.newUpdateMarkdownCommand())
	command.AddCommand(factory.newUpdateWhiteboardCommand("html", http.WhiteboardHTML))
	return command
}

func (factory commandFactory) newUpdateMarkdownCommand() *cobra.Command {
	command := &cobra.Command{Use: "markdown <id> <file>", Args: usageArgs(cobra.ExactArgs(2))}
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
		openedSource, source, openedContext, creatorContext, err := openMarkdownPair(args[1], *contextPath)
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
		updated, err := client.UpdateMarkdown(ctx, args[0], source, creatorContext, expiration)
		if err != nil {
			return stableCommandError(err)
		}
		return stableCommandError(writeResource(factory.deps.Stdout, factory.root.json, client, updated))
	}
	return command
}

func (factory commandFactory) newUpdateWhiteboardCommand(name string, kind http.WhiteboardKind) *cobra.Command {
	command := &cobra.Command{Use: name + " <id> <file>", Args: usageArgs(cobra.ExactArgs(2))}
	expires := expirationFlag(command)
	command.RunE = func(cmd *cobra.Command, args []string) error {
		expiration, err := resolveExpiration(cmd, *expires)
		if err != nil {
			return err
		}
		opened, file, err := openRegularFile(args[1])
		if err != nil {
			return err
		}
		defer opened.Close()

		client, ctx, cancel, err := factory.newClient(cmd)
		if err != nil {
			return err
		}
		defer cancel()
		updated, err := client.UpdateWhiteboard(ctx, kind, args[0], file, expiration)
		if err != nil {
			return stableCommandError(err)
		}
		return stableCommandError(writeResource(factory.deps.Stdout, factory.root.json, client, updated))
	}
	return command
}

func (factory commandFactory) newGetCommand() *cobra.Command {
	command := &cobra.Command{Use: "get", Args: usageArgs(cobra.NoArgs)}
	command.AddCommand(&cobra.Command{
		Use:  "markdown <id>",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !factory.root.json {
				return invalidCommand("get markdown requires --json")
			}
			client, ctx, cancel, err := factory.newClient(cmd)
			if err != nil {
				return err
			}
			defer cancel()
			response, err := client.GetMarkdown(ctx, args[0])
			if err != nil {
				return stableCommandError(err)
			}
			return stableCommandError(writeMarkdown(factory.deps.Stdout, client, response))
		},
	})
	return command
}

func (factory commandFactory) newDeleteCommand() *cobra.Command {
	command := &cobra.Command{Use: "delete", Args: usageArgs(cobra.NoArgs)}
	command.AddCommand(factory.newDeleteWhiteboardCommand("markdown", http.WhiteboardMarkdown))
	command.AddCommand(factory.newDeleteWhiteboardCommand("html", http.WhiteboardHTML))
	return command
}

func (factory commandFactory) newDeleteWhiteboardCommand(name string, kind http.WhiteboardKind) *cobra.Command {
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

func openMarkdownPair(sourcePath, contextPath string) (*os.File, http.File, *os.File, http.File, error) {
	openedSource, source, err := openRegularFile(sourcePath)
	if err != nil {
		return nil, http.File{}, nil, http.File{}, err
	}
	openedContext, creatorContext, err := openRegularFile(contextPath)
	if err != nil {
		_ = openedSource.Close()
		return nil, http.File{}, nil, http.File{}, err
	}
	return openedSource, source, openedContext, creatorContext, nil
}
