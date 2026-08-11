package cli

import (
	"errors"
	"os"

	"github.com/edocsss/agent-whiteboard/internal/webapi"
	"github.com/spf13/cobra"
)

func (factory commandFactory) newImageCommand() *cobra.Command {
	command := &cobra.Command{Use: "image", Args: usageArgs(cobra.NoArgs)}
	command.AddCommand(factory.newImageUploadCommand(), factory.newImageUpdateCommand(), factory.newImageDeleteCommand())
	return command
}

func (factory commandFactory) newImageUploadCommand() *cobra.Command {
	command := &cobra.Command{Use: "upload <files...>", Args: usageArgs(cobra.MinimumNArgs(1))}
	expires := expirationFlag(command)
	command.RunE = func(cmd *cobra.Command, args []string) error {
		expiration, err := resolveExpiration(cmd, *expires)
		if err != nil {
			return err
		}
		opened := make([]*os.File, 0, len(args))
		files := make([]webapi.File, 0, len(args))
		closeFiles := func() error {
			var closeErr error
			for _, file := range opened {
				closeErr = errors.Join(closeErr, file.Close())
			}
			return closeErr
		}
		for _, path := range args {
			file, input, openErr := openRegularFile(path)
			if openErr != nil {
				_ = closeFiles()
				return openErr
			}
			opened = append(opened, file)
			files = append(files, input)
		}
		defer closeFiles()

		client, ctx, cancel, err := factory.newClient(cmd)
		if err != nil {
			return err
		}
		defer cancel()
		created, err := client.CreateImages(ctx, files, expiration)
		if err != nil {
			return stableCommandError(err)
		}
		return stableCommandError(writeResourceList(factory.deps.Stdout, factory.root.json, client, created))
	}
	return command
}

func (factory commandFactory) newImageUpdateCommand() *cobra.Command {
	command := &cobra.Command{Use: "update <id> <file>", Args: usageArgs(cobra.ExactArgs(2))}
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
		updated, err := client.UpdateImage(ctx, args[0], file, expiration)
		if err != nil {
			return stableCommandError(err)
		}
		return stableCommandError(writeResource(factory.deps.Stdout, factory.root.json, client, updated))
	}
	return command
}

func (factory commandFactory) newImageDeleteCommand() *cobra.Command {
	return &cobra.Command{
		Use:  "delete <id>",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, ctx, cancel, err := factory.newClient(cmd)
			if err != nil {
				return err
			}
			defer cancel()
			if err := client.DeleteImage(ctx, args[0]); err != nil {
				return stableCommandError(err)
			}
			return writeDeleteSuccess(factory.deps.Stdout, factory.root.json)
		},
	}
}
