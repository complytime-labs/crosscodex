package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
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
  - OSCAL JSON (.json)
  - Raw documents (with --raw flag)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			filePath := args[0]
			content, err := os.ReadFile(filePath)
			if err != nil {
				return fmt.Errorf("failed to read file: %w", err)
			}

			raw, _ := cmd.Flags().GetBool("raw")

			req := &pb.SubmitDocumentRequest{
				Source: &pb.SubmitDocumentRequest_Content{
					Content: content,
				},
			}

			if !raw {
				req.CatalogFormat = pb.CatalogFormat_CATALOG_FORMAT_OSCAL
			}

			resp, err := state.client.SubmitDocument(cmd.Context(), req)
			if err != nil {
				return fmt.Errorf("failed to submit document: %w", err)
			}

			return emit(cmd,
				func(w io.Writer, color bool) {
					fmt.Fprintf(w, "Document submitted successfully\n")
					fmt.Fprintf(w, "Job ID: %s\n", resp.GetJobId())
					fmt.Fprintf(w, "Document ID: %s\n", resp.GetDocumentId())
					fmt.Fprintf(w, "Status: %s\n", resp.GetStatus().String())
				},
				map[string]string{
					"job_id":      resp.GetJobId(),
					"document_id": resp.GetDocumentId(),
					"status":      resp.GetStatus().String(),
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
			"crosscodex catalog import --raw custom-controls.txt",
		},
	})

	cmd.Flags().Bool("raw", false, "Import as raw document (no OSCAL parsing)")

	return cmd
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

			resp, err := state.client.ListCatalogs(cmd.Context(), req)
			if err != nil {
				return fmt.Errorf("failed to list catalogs: %w", err)
			}

			catalogs := resp.GetCatalogs()
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

	cmd.Flags().String("tenant", "", "Filter by tenant context")

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

			resp, err := state.client.GetCatalog(cmd.Context(), req)
			if err != nil {
				return fmt.Errorf("failed to get catalog: %w", err)
			}

			cat := resp.GetCatalog()

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

	cmd.Flags().String("tenant", "", "Tenant context for the catalog")

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

			var data map[string]any
			if err := json.Unmarshal(content, &data); err != nil {
				return fmt.Errorf("invalid JSON: %w", err)
			}

			if _, ok := data["catalog"]; !ok {
				return errors.New("missing required 'catalog' key in OSCAL structure")
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
