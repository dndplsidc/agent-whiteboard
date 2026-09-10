package cli

import (
	"context"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/dndplsidc/agent-whiteboard/internal/catalog"
	"github.com/dndplsidc/agent-whiteboard/internal/common"
	generalconfig "github.com/dndplsidc/agent-whiteboard/internal/config"
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
	metadata := metadataFlags(command)
	command.RunE = func(cmd *cobra.Command, args []string) error {
		resolvedMetadata, err := metadata.resolve(command, true)
		if err != nil {
			return err
		}
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

		settings, err := factory.resolveClientSettings(cmd)
		if err != nil {
			return err
		}
		server, err := generalconfig.CanonicalPublishingOrigin(settings.server)
		if err != nil {
			return invalidCommand("server must be an absolute HTTP origin")
		}
		local, err := factory.newCatalog()
		if err != nil {
			return catalogLocalError{code: "catalog_unavailable", message: "Local catalog is unavailable; no whiteboard was published."}
		}
		if err := local.Prepare(cmd.Context(), server, catalogKind(kind)); err != nil {
			return catalogPreparationError(err, "Local catalog is not writable; no whiteboard was published.")
		}
		client, ctx, cancel, err := factory.newClientFromSettings(cmd, settings)
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
		return factory.finishCreate(cmd.Context(), local, client, server, kind, resolvedMetadata, source.Name, created, err)
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
	metadata := metadataFlags(command)
	command.RunE = func(cmd *cobra.Command, args []string) error {
		resolvedMetadata, err := metadata.resolve(command, false)
		if err != nil {
			return err
		}
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

		if err := common.ValidateID(args[0]); err != nil {
			return stableCommandError(err)
		}
		client, settings, ctx, cancel, err := factory.newClientWithSettings(cmd)
		if err != nil {
			return err
		}
		defer cancel()
		server, err := generalconfig.CanonicalPublishingOrigin(settings.server)
		if err != nil {
			return invalidCommand("server must be an absolute HTTP origin")
		}
		local, err := factory.newCatalog()
		if err != nil {
			return catalogLocalError{code: "catalog_unavailable", message: "Local catalog is unavailable; no whiteboard update was sent."}
		}
		identity := catalog.Identity{Server: server, Kind: catalogKind(kind), ID: args[0]}
		entry, found, err := local.OpenExisting(ctx, identity)
		if err != nil {
			return catalogPreparationError(err, "Local catalog could not be prepared; no whiteboard update was sent.")
		}
		if found {
			defer entry.Close()
		}
		var updated webapi.Resource
		if kind == webapi.WhiteboardMarkdown {
			updated, err = client.UpdateMarkdown(ctx, args[0], source, creatorContext, expiration)
		} else {
			updated, err = client.UpdateHTML(ctx, args[0], source, creatorContext, expiration)
		}
		if err != nil {
			return stableCommandError(err)
		}
		resolved, err := resolveJSONResources(client, []webapi.Resource{updated})
		if err != nil {
			return stableCommandError(err)
		}
		if found {
			if err := entry.Update(context.WithoutCancel(cmd.Context()), catalog.Update{
				URL: resolved[0].URL, SourceFilename: source.Name,
				Title: resolvedMetadata.title, Summary: resolvedMetadata.summary,
				ExpiresAt: updated.ExpiresAt, Permanent: updated.Permanent,
			}); err != nil {
				writeCatalogWarning(factory.deps.Stderr, factory.root.json, "catalog_write_failed", "Whiteboard was updated, but its local catalog record could not be saved.")
			}
		} else if resolvedMetadata.supplied() {
			writeCatalogWarning(factory.deps.Stderr, factory.root.json, "catalog_record_missing", "Whiteboard was updated, but title and summary metadata were not recorded because this board is not in the local catalog.")
		}
		return stableCommandError(writeResolvedResource(factory.deps.Stdout, factory.root.json, resolved[0]))
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
			if err := common.ValidateID(args[0]); err != nil {
				return stableCommandError(err)
			}
			client, settings, ctx, cancel, err := factory.newClientWithSettings(cmd)
			if err != nil {
				return err
			}
			defer cancel()
			server, err := generalconfig.CanonicalPublishingOrigin(settings.server)
			if err != nil {
				return invalidCommand("server must be an absolute HTTP origin")
			}
			local, err := factory.newCatalog()
			if err != nil {
				return catalogLocalError{code: "catalog_unavailable", message: "Local catalog is unavailable; no whiteboard deletion was sent."}
			}
			entry, found, err := local.OpenExisting(ctx, catalog.Identity{Server: server, Kind: catalogKind(kind), ID: args[0]})
			if err != nil {
				return catalogPreparationError(err, "Local catalog could not be prepared; no whiteboard deletion was sent.")
			}
			if found {
				defer entry.Close()
			}
			if err := client.DeleteWhiteboard(ctx, kind, args[0]); err != nil {
				return stableCommandError(err)
			}
			if found {
				if err := entry.MarkDeleted(context.WithoutCancel(cmd.Context())); err != nil {
					writeCatalogWarning(factory.deps.Stderr, factory.root.json, "catalog_write_failed", "Whiteboard was deleted, but its local catalog record could not be saved.")
				}
			}
			return writeDeleteSuccess(factory.deps.Stdout, factory.root.json)
		},
	}
}

func (factory commandFactory) finishCreate(commandContext context.Context, local Catalog, client Client, server string, kind webapi.WhiteboardKind, metadata resolvedMetadata, sourceFilename string, created webapi.Resource, createErr error) error {
	if createErr != nil && created.ID == "" {
		return stableCommandError(createErr)
	}
	resolved, err := resolveJSONResources(client, []webapi.Resource{created})
	if err != nil {
		if createErr != nil {
			return stableCommandError(createErr)
		}
		return stableCommandError(err)
	}
	state := catalog.StateCreated
	if createErr != nil {
		state = catalog.StateCreationUncertain
	}
	if err := local.RecordCreation(context.WithoutCancel(commandContext), catalog.Creation{
		Server: server, Kind: catalogKind(kind), ID: created.ID, URL: resolved[0].URL,
		Title: metadata.titleValue(), Summary: metadata.summaryValue(), SourceFilename: sourceFilename,
		ExpiresAt: created.ExpiresAt, Permanent: created.Permanent, State: state,
	}); err != nil {
		writeCatalogWarning(factory.deps.Stderr, factory.root.json, "catalog_write_failed", "Whiteboard was published, but its local catalog record could not be saved.")
	}
	writeErr := writeResolvedResource(factory.deps.Stdout, factory.root.json, resolved[0])
	if createErr != nil {
		return stableCommandError(createErr)
	}
	return stableCommandError(writeErr)
}

type metadataFlagValues struct {
	title   string
	summary string
}

type resolvedMetadata struct {
	title   *string
	summary *string
}

func metadataFlags(command *cobra.Command) *metadataFlagValues {
	values := &metadataFlagValues{}
	command.Flags().StringVar(&values.title, "title", "", "local catalog title")
	command.Flags().StringVar(&values.summary, "summary", "", "local catalog summary")
	return values
}

func (values *metadataFlagValues) resolve(command *cobra.Command, required bool) (resolvedMetadata, error) {
	title, titleSet, err := resolveMetadataValue(command, "title", values.title, required)
	if err != nil {
		return resolvedMetadata{}, err
	}
	summary, _, err := resolveMetadataValue(command, "summary", values.summary, required)
	if err != nil {
		return resolvedMetadata{}, err
	}
	if !titleSet && !command.Flags().Changed("summary") {
		return resolvedMetadata{}, nil
	}
	return resolvedMetadata{title: title, summary: summary}, nil
}

func resolveMetadataValue(command *cobra.Command, name, value string, required bool) (*string, bool, error) {
	set := command.Flags().Changed(name)
	if required && !set {
		return nil, false, invalidCommand(name + " is required")
	}
	if !set {
		return nil, false, nil
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || !utf8.ValidString(value) {
		return nil, true, invalidCommand(name + " must be non-empty UTF-8")
	}
	return &trimmed, true, nil
}

func (metadata resolvedMetadata) supplied() bool {
	return metadata.title != nil || metadata.summary != nil
}
func (metadata resolvedMetadata) titleValue() string   { return *metadata.title }
func (metadata resolvedMetadata) summaryValue() string { return *metadata.summary }

func catalogKind(kind webapi.WhiteboardKind) catalog.Kind {
	if kind == webapi.WhiteboardHTML {
		return catalog.KindHTML
	}
	return catalog.KindMarkdown
}

func catalogPreparationError(err error, message string) error {
	if _, contextOnly := contextOnlyError(err); contextOnly {
		return stableCommandError(err)
	}
	return catalogLocalError{code: "catalog_unavailable", message: message}
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
