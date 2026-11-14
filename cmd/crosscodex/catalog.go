package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"

	connectrpc "connectrpc.com/connect"
	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/catalog"
	"github.com/spf13/cobra"
)

func newCatalogCmd(state *cliState) *cobra.Command {
	catalogCmd := &cobra.Command{
		Use:   "catalog",
		Short: "Import, inspect, and validate compliance catalogs",
		RunE:  func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}

	catalogCmd.AddCommand(newCatalogImportCmd(state))
	catalogCmd.AddCommand(newCatalogListCmd(state))
	catalogCmd.AddCommand(newCatalogInspectCmd(state))
	catalogCmd.AddCommand(newCatalogValidateCmd(state))

	return catalogCmd
}

func newCatalogImportCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "import <file>",
		Aliases: []string{"add"},
		Short:   "Import a compliance catalog from a local file",
		Long: `Import a compliance catalog from a local file.

Supported formats:
  - OSCAL JSON catalogs (.json)

With --raw the server detects the format from the content instead of
assuming OSCAL. Only OSCAL JSON is accepted today; other documents are
rejected as an unsupported catalog format.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			filePath := args[0]
			raw, _ := cmd.Flags().GetBool("raw")

			chunkSize := 1048576 // 1MB default
			if state.cfg != nil && state.cfg.UploadChunkSize > 0 {
				chunkSize = state.cfg.UploadChunkSize
			}

			file, err := os.Open(filePath)
			if err != nil {
				return fmt.Errorf("failed to open file: %w", err)
			}
			defer file.Close()

			stream := state.client.StreamDocument(cmd.Context())

			catalogFormat := pb.CatalogFormat_CATALOG_FORMAT_OSCAL
			if raw {
				catalogFormat = pb.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED
			}

			if err := stream.Send(&pb.StreamDocumentChunk{
				Payload: &pb.StreamDocumentChunk_Metadata{
					Metadata: &pb.StreamDocumentMetadata{
						CatalogFormat: catalogFormat,
					},
				},
			}); err != nil {
				return fmt.Errorf("failed to send metadata: %w", err)
			}

			buf := make([]byte, chunkSize)
			for {
				n, readErr := file.Read(buf)
				if n > 0 {
					if err := stream.Send(&pb.StreamDocumentChunk{
						Payload: &pb.StreamDocumentChunk_Chunk{
							Chunk: buf[:n],
						},
					}); err != nil {
						return fmt.Errorf("failed to send chunk: %w", err)
					}
				}
				if readErr == io.EOF {
					break
				}
				if readErr != nil {
					return fmt.Errorf("failed to read file: %w", readErr)
				}
			}

			resp, err := stream.CloseAndReceive()
			if err != nil {
				return fmt.Errorf("failed to import catalog: %w", err)
			}

			catalogID := resp.Msg.GetCatalogId()
			textless := textlessResults(resp.Msg.GetTextlessControls())
			return emit(cmd,
				func(w io.Writer, color bool) {
					fmt.Fprintf(w, "Catalog imported\n")
					fmt.Fprintf(w, "Catalog ID: %s\n", catalogID)
					fmt.Fprintf(w, "Document ID: %s\n", resp.Msg.GetDocumentId())
					printTextlessControls(w, textless)
					fmt.Fprintf(w, "Next: crosscodex run start --source %s --target <catalog-id>\n", catalogID)
				},
				catalogImportResult{
					CatalogID:        catalogID,
					DocumentID:       resp.Msg.GetDocumentId(),
					TextlessControls: textless,
				},
			)
		},
	}

	guidedArgs(cmd, cobra.ExactArgs(1), argGuide{
		noun:  "a file path",
		find:  "crosscodex catalog import <file>",
		usage: "crosscodex catalog import <file>",
		examples: []string{
			"crosscodex catalog import nist-800-53.json",
			"crosscodex catalog import --raw nist-800-53.json",
		},
	})

	cmd.Flags().Bool("raw", false, "Let the server detect the format instead of assuming OSCAL")

	return cmd
}

// catalogImportResult is the --json output of catalog import.
type catalogImportResult struct {
	CatalogID  string `json:"catalog_id"`
	DocumentID string `json:"document_id"`
	// TextlessControls lists every control imported without requirement text;
	// an empty array, never null, when there are none.
	TextlessControls []textlessResult `json:"textless_controls"`
}

type textlessResult struct {
	Identifier string   `json:"identifier"`
	PartNames  []string `json:"part_names"`
}

func textlessResults(controls []*pb.TextlessControl) []textlessResult {
	out := make([]textlessResult, len(controls))
	for i, c := range controls {
		out[i] = textlessResult{Identifier: c.GetIdentifier(), PartNames: append([]string{}, c.GetPartNames()...)}
	}
	return out
}

// maxTextlessListed bounds the identifiers the human output names; --json
// carries the full list.
const maxTextlessListed = 10

// terminalSafe escapes every non-printable rune of a catalog-derived string
// (ESC, newline, C1 controls, bidi overrides such as U+202E, zero-width and
// line-separator runes) as \x1b, \n, \u202e and the like, so a hostile catalog
// cannot spoof or hide terminal output. Printable text, including non-ASCII
// letters, is unchanged.
func terminalSafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		if !unicode.IsPrint(r) {
			q := strconv.QuoteRune(r)
			b.WriteString(q[1 : len(q)-1])
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// printTextlessControls tells the user which imported controls will not be
// analysed and how to fix it. It prints nothing when there are none.
func printTextlessControls(w io.Writer, controls []textlessResult) {
	if len(controls) == 0 {
		return
	}
	listed := make([]string, 0, maxTextlessListed)
	parts := map[string]struct{}{}
	for i, c := range controls {
		if i < maxTextlessListed {
			listed = append(listed, terminalSafe(c.Identifier))
		}
		for _, name := range c.PartNames {
			parts[terminalSafe(name)] = struct{}{}
		}
	}

	subject := fmt.Sprintf("%d controls have", len(controls))
	if len(controls) == 1 {
		subject = "1 control has"
	}
	more := "."
	if rest := len(controls) - len(listed); rest > 0 {
		more = fmt.Sprintf(", and %d more.", rest)
	}
	fmt.Fprintf(w, "%s no requirement text and will not be analysed: %s%s\n", subject, strings.Join(listed, ", "), more)

	if len(parts) > 0 {
		names := make([]string, 0, len(parts))
		for name := range parts {
			names = append(names, name)
		}
		sort.Strings(names)
		fmt.Fprintf(w, "Their parts are named: %s. If one of these holds the requirement, add it to catalog.requirement_part_names and re-import.\n", strings.Join(names, ", "))
	}
}

func newCatalogListCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List all catalogs",
		Long:    `List all compliance catalogs in the system.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			tenant, _ := cmd.Flags().GetString("tenant")

			req := &pb.ListCatalogsRequest{}
			if tenant != "" {
				req.TenantContext = &pb.TenantContext{
					TenantId: tenant,
				}
			}

			resp, err := state.client.ListCatalogs(cmd.Context(), connectrpc.NewRequest(req))
			if err != nil {
				return fmt.Errorf("failed to list catalogs: %w", err)
			}

			catalogs := resp.Msg.GetCatalogs()
			if len(catalogs) == 0 {
				return emit(cmd,
					func(w io.Writer, color bool) {
						fmt.Fprintln(w, "No catalogs found")
					},
					[]any{},
				)
			}

			return emit(cmd,
				func(w io.Writer, color bool) {
					tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
					fmt.Fprintln(tw, "ID\tNAME\tCONTROLS\tFORMAT")
					for _, cat := range catalogs {
						format := cat.GetFormat().String()
						switch format {
						case "CATALOG_FORMAT_OSCAL":
							format = "OSCAL"
						case "CATALOG_FORMAT_GEMARA":
							format = "GEMARA"
						}
						fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n",
							cat.GetCatalogId(),
							cat.GetName(),
							cat.GetControlCount(),
							format,
						)
					}
					if err := tw.Flush(); err != nil {
						fmt.Fprintf(w, "Error flushing output: %v\n", err)
					}
				},
				catalogsToJSON(catalogs),
			)
		},
	}

	cmd.Flags().String("tenant", "", "Tenant to act on; must be the tenant your client certificate authenticates (default: that tenant)")

	return cmd
}

func newCatalogInspectCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "inspect <id>",
		Aliases: []string{"show", "describe"},
		Short:   "Inspect a catalog by ID",
		Long:    `Display detailed information about a specific catalog.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			catalogID := args[0]
			tenant, _ := cmd.Flags().GetString("tenant")

			req := &pb.GetCatalogRequest{
				CatalogId: catalogID,
			}
			if tenant != "" {
				req.TenantContext = &pb.TenantContext{
					TenantId: tenant,
				}
			}

			resp, err := state.client.GetCatalog(cmd.Context(), connectrpc.NewRequest(req))
			if err != nil {
				return fmt.Errorf("failed to get catalog: %w", err)
			}

			cat := resp.Msg.GetCatalog()

			return emit(cmd,
				func(w io.Writer, color bool) {
					fmt.Fprintf(w, "Catalog: %s\n", cat.GetName())
					fmt.Fprintf(w, "ID: %s\n", cat.GetCatalogId())
					fmt.Fprintf(w, "Format: %s\n", cat.GetFormat().String())
					fmt.Fprintf(w, "Version: %s\n", cat.GetVersion())
					fmt.Fprintf(w, "Controls: %d\n", cat.GetControlCount())
					if len(cat.GetProperties()) > 0 {
						fmt.Fprintln(w, "\nProperties:")
						for k, v := range cat.GetProperties() {
							fmt.Fprintf(w, "  %s: %s\n", k, v)
						}
					}
				},
				catalogToJSON(cat),
			)
		},
	}

	guidedArgs(cmd, cobra.ExactArgs(1), argGuide{
		noun:  "a catalog ID",
		find:  "crosscodex catalog list",
		usage: "crosscodex catalog inspect <id>",
		examples: []string{
			"crosscodex catalog inspect cat_01234567890",
			"crosscodex catalog show cat_01234567890",
			"crosscodex catalog describe cat_01234567890 --tenant acme",
		},
	})

	cmd.Flags().String("tenant", "", "Tenant to act on; must be the tenant your client certificate authenticates (default: that tenant)")

	return cmd
}

func newCatalogValidateCmd(_ *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate <file>",
		Short: "Validate a catalog file locally",
		Long: `Validate a catalog file against OSCAL structure locally.

This command performs basic JSON validation and checks for required OSCAL
structure. It does not require a running daemon.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			filePath := args[0]
			content, err := os.ReadFile(filePath)
			if err != nil {
				return fmt.Errorf("failed to read file: %w", err)
			}

			// Classify a rejected file as import does, so validate gives the
			// same advice (profile, XML, YAML, broken JSON) before an upload.
			var data map[string]any
			if err := json.Unmarshal(content, &data); err != nil {
				return catalog.UnsupportedFormatError(pb.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED, content)
			}
			if _, ok := data[catalog.OSCALRootKey]; !ok {
				return catalog.UnsupportedFormatError(pb.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED, content)
			}

			return emit(cmd,
				func(w io.Writer, color bool) {
					fmt.Fprintf(w, "✓ File is valid OSCAL catalog JSON\n")
					fmt.Fprintf(w, "  Path: %s\n", filePath)
				},
				map[string]string{
					"valid": "true",
					"path":  filePath,
				},
			)
		},
	}

	guidedArgs(cmd, cobra.ExactArgs(1), argGuide{
		noun:  "a file path",
		find:  "crosscodex catalog validate <file>",
		usage: "crosscodex catalog validate <file>",
		examples: []string{
			"crosscodex catalog validate nist-800-53.json",
			"crosscodex catalog validate oscal-catalog.json",
		},
	})

	return cmd
}

func catalogToJSON(cat *pb.Catalog) map[string]any {
	return map[string]any{
		"catalog_id":    cat.GetCatalogId(),
		"name":          cat.GetName(),
		"format":        cat.GetFormat().String(),
		"version":       cat.GetVersion(),
		"control_count": cat.GetControlCount(),
		"properties":    cat.GetProperties(),
	}
}

func catalogsToJSON(catalogs []*pb.Catalog) []map[string]any {
	result := make([]map[string]any, len(catalogs))
	for i, cat := range catalogs {
		result[i] = catalogToJSON(cat)
	}
	return result
}
