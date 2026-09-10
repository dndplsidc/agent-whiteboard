package cli

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/dndplsidc/agent-whiteboard/internal/catalog"
	"github.com/spf13/cobra"
)

func (factory commandFactory) newCatalogCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "catalog",
		Short: "Search whiteboards recorded by this local CLI",
		Args:  usageArgs(cobra.NoArgs),
	}
	command.AddCommand(factory.newCatalogListCommand())
	return command
}

func (factory commandFactory) newCatalogListCommand() *cobra.Command {
	var kindText, terms string
	var limit, offset int
	command := &cobra.Command{
		Use:         "list",
		Short:       "List local records offline across all publishing servers",
		Long:        "List whiteboards recorded by this local CLI. The command searches the local catalog offline across all recorded publishing servers; --server and the configured client server do not filter it.",
		Args:        usageArgs(cobra.NoArgs),
		Annotations: map[string]string{handlesConfigurationAnnotation: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			kind := catalog.Kind(kindText)
			if kind != "" && kind != catalog.KindMarkdown && kind != catalog.KindHTML {
				return invalidCommand("kind must be markdown or html")
			}
			if limit <= 0 {
				return invalidCommand("limit must be positive")
			}
			if offset < 0 {
				return invalidCommand("offset must not be negative")
			}
			local, err := factory.newCatalog()
			if err != nil {
				return err
			}
			page, err := local.List(cmd.Context(), catalog.Query{Kind: kind, Terms: terms, Limit: limit, Offset: offset})
			if err != nil {
				return catalogLocalError{code: "catalog_read_failed", message: "Local catalog could not be read; inspect or repair the malformed record."}
			}
			return writeCatalogList(factory.deps.Stdout, factory.root.json, page)
		},
	}
	command.Flags().StringVar(&kindText, "kind", "", "filter by markdown or html")
	command.Flags().StringVar(&terms, "query", "", "search title and summary using all terms")
	command.Flags().IntVar(&limit, "limit", 20, "maximum records to return")
	command.Flags().IntVar(&offset, "offset", 0, "matching records to skip")
	return command
}

type noopCatalog struct{}

func (noopCatalog) Prepare(context.Context, string, catalog.Kind) error    { return nil }
func (noopCatalog) RecordCreation(context.Context, catalog.Creation) error { return nil }
func (noopCatalog) OpenExisting(context.Context, catalog.Identity) (*catalog.Entry, bool, error) {
	return nil, false, nil
}
func (noopCatalog) List(_ context.Context, query catalog.Query) (catalog.Page, error) {
	return catalog.Page{Records: []catalog.ResultRecord{}, Limit: query.Limit, Offset: query.Offset}, nil
}

func safeTerminal(value string) string {
	var output strings.Builder
	for _, character := range value {
		if unicode.IsControl(character) {
			output.WriteString(strconv.QuoteRune(character))
			continue
		}
		output.WriteRune(character)
	}
	return output.String()
}

func humanCatalogTime(seconds int64) string {
	value := time.Unix(seconds, 0).UTC()
	if value.Year() < 0 || value.Year() > 9999 {
		return strconv.FormatInt(seconds, 10)
	}
	return value.Format(time.RFC3339)
}
