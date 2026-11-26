package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"

	connectrpc "connectrpc.com/connect"
	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
	"github.com/complytime-labs/crosscodex/internal/catalog"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/pkg/catalogversion"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
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
	catalogCmd.AddCommand(newCatalogControlsCmd(state))
	catalogCmd.AddCommand(newCatalogValidateCmd(state))
	catalogCmd.AddCommand(newCatalogContentHashCmd(state))
	catalogCmd.AddCommand(newCatalogRetireCmd(state))

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
  - PDF, DOCX, HTML and other documents, when the server has ingestion
    configured (ingestion.address): crosscodex-ingestion converts them and
    the server splits the result into controls. Without it they are
    rejected as an unsupported catalog format.
  - Structured JSON, CSV or XLSX sources, with --mapping naming a mapping
    registered on the ingestion server (e.g. owasp-asvs-5.0.0-json):
    crosscodex-ingestion reads the file through it, keeping the source's
    own control IDs and groups. Needs ingestion configured, as above.

A file that is OSCAL JSON is sent as OSCAL; any other file lets the server
detect the format. With --raw or --mapping the server detects the format of
every file, OSCAL JSON included.

With --document the catalog is stored as a version of that document. Import
the next edition of the same document under the same slug and it becomes the
current version; the output counts the controls added, removed and changed
against the version it superseded (--json lists their identifiers). --version-label names the version (for
example "Rev 5"); without it the label is the OSCAL metadata version, else the
import date and a short content hash. Content already stored under another
document cannot be moved to this one.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			filePath := args[0]
			raw, _ := cmd.Flags().GetBool("raw")
			documentSlug, _ := cmd.Flags().GetString("document")
			versionLabel, _ := cmd.Flags().GetString("version-label")
			mapping, _ := cmd.Flags().GetString("mapping")

			// The server validates again; checking here fails before the upload.
			if documentSlug != "" {
				if err := catalog.ValidateDocumentSlug(documentSlug); err != nil {
					return fmt.Errorf("--document: %w", err)
				}
			}
			if err := gateway.ValidateIngestionMapping(mapping); err != nil {
				return fmt.Errorf("--mapping: %w", err)
			}
			if versionLabel != "" {
				if documentSlug == "" {
					return fmt.Errorf("--version-label requires --document: a label names a version of a document. Add --document <slug>, or drop --version-label")
				}
				if err := catalog.ValidateVersionLabel(versionLabel); err != nil {
					return fmt.Errorf("--version-label: %w", err)
				}
			}

			chunkSize := 1048576 // 1MB default
			if state.cfg != nil && state.cfg.UploadChunkSize > 0 {
				chunkSize = state.cfg.UploadChunkSize
			}

			file, err := os.Open(filePath)
			if err != nil {
				return fmt.Errorf("failed to open file: %w", err)
			}
			defer file.Close()

			// OSCAL JSON (of any model, so the server can explain a
			// non-catalog one) is sent as OSCAL; anything else, and every
			// file under --raw, lets the server detect the format, which is
			// what routes a PDF or other document to conversion. A mapped
			// file is ingestion's to read, whatever it looks like.
			var content io.Reader = file
			catalogFormat := pb.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED
			if !raw && mapping == "" {
				data, err := io.ReadAll(file)
				if err != nil {
					return fmt.Errorf("failed to read file: %w", err)
				}
				if oscal.IsModelJSON(data) {
					catalogFormat = pb.CatalogFormat_CATALOG_FORMAT_OSCAL
				}
				content = bytes.NewReader(data)
			}

			stream := state.client.StreamDocument(cmd.Context())

			if err := stream.Send(&pb.StreamDocumentChunk{
				Payload: &pb.StreamDocumentChunk_Metadata{
					Metadata: &pb.StreamDocumentMetadata{
						CatalogFormat:    catalogFormat,
						DocumentSlug:     documentSlug,
						VersionLabel:     versionLabel,
						IngestionMapping: mapping,
					},
				},
			}); err != nil {
				return fmt.Errorf("failed to send metadata: %w", err)
			}

			buf := make([]byte, chunkSize)
			for {
				n, readErr := content.Read(buf)
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
			version := terminalSafe(resp.Msg.GetVersion())
			superseded := resp.Msg.GetSupersededCatalogId()
			changes := toChangeReportResult(resp.Msg.GetChangeReport())
			return emit(cmd,
				func(w io.Writer, color bool) {
					fmt.Fprintf(w, "Catalog imported\n")
					fmt.Fprintf(w, "Catalog ID: %s\n", catalogID)
					fmt.Fprintf(w, "Document ID: %s\n", resp.Msg.GetDocumentId())
					printVersionOutcome(w, version, superseded, changes)
					printTextlessControls(w, textless)
					fmt.Fprintf(w, "Next: crosscodex run start --source %s --target <catalog-id>\n", catalogID)
				},
				catalogImportResult{
					CatalogID:           catalogID,
					DocumentID:          resp.Msg.GetDocumentId(),
					TextlessControls:    textless,
					VersionDisplay:      resp.Msg.GetVersion(),
					SupersededCatalogID: superseded,
					ChangeReport:        changes,
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
			"crosscodex catalog import --document nist-800-53 --version-label \"Rev 5\" nist-800-53.json",
			"crosscodex catalog import --document asvs --mapping owasp-asvs-5.0.0-json asvs.json",
		},
	})

	cmd.Flags().Bool("raw", false, "Let the server detect the format even of OSCAL JSON, instead of sending it as OSCAL")
	cmd.Flags().String("document", "", "Store the catalog as a version of this document (slug: a-z, 0-9 and '-', e.g. nist-800-53); re-import under the same slug to supersede the current version")
	cmd.Flags().String("mapping", "", "Read a structured JSON, CSV or XLSX source through this mapping, the ID of a mapping registered on the ingestion server (e.g. owasp-asvs-5.0.0-json)")
	cmd.Flags().String("version-label", "", "Name this version, e.g. \"Rev 5\" (needs --document; default: the OSCAL metadata version, else import date and content hash)")

	return cmd
}

// catalogImportResult is the --json output of catalog import.
type catalogImportResult struct {
	CatalogID  string `json:"catalog_id"`
	DocumentID string `json:"document_id"`
	// TextlessControls lists every control imported without requirement text;
	// an empty array, never null, when there are none.
	TextlessControls []textlessResult `json:"textless_controls"`
	// VersionDisplay is the "slug @ label (date · hash)" form, the same key
	// catalog list and inspect use; empty, and omitted, when the catalog was
	// imported without --document. (Not "version": there it is the OSCAL
	// metadata version.)
	VersionDisplay string `json:"version_display,omitempty"`
	// SupersededCatalogID is the catalog this version replaced as the
	// document's current version; omitted for a document's first version.
	SupersededCatalogID string `json:"superseded_catalog_id,omitempty"`
	// ChangeReport lists control identifiers that differ from the superseded
	// version; omitted when nothing was superseded.
	ChangeReport *changeReportResult `json:"change_report,omitempty"`
}

// changeReportResult mirrors pb.ChangeReport. The lists are never null.
type changeReportResult struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	Changed []string `json:"changed"`
	// IDsUnstable is true when the versions share too few identifiers for the
	// lists to read as an edit history.
	IDsUnstable bool `json:"ids_unstable"`
}

func toChangeReportResult(r *pb.ChangeReport) *changeReportResult {
	if r == nil {
		return nil
	}
	return &changeReportResult{
		Added:       append([]string{}, r.GetAdded()...),
		Removed:     append([]string{}, r.GetRemoved()...),
		Changed:     append([]string{}, r.GetChanged()...),
		IDsUnstable: r.GetIdsUnstable(),
	}
}

// printVersionOutcome prints the version, the catalog it superseded and the
// change counts. It prints nothing for an import without --document.
func printVersionOutcome(w io.Writer, version, superseded string, changes *changeReportResult) {
	if version == "" {
		return
	}
	fmt.Fprintf(w, "Version: %s\n", version)
	if superseded == "" {
		return
	}
	fmt.Fprintf(w, "Supersedes: %s\n", terminalSafe(superseded))
	if changes == nil {
		return
	}
	fmt.Fprintf(w, "Changes: %d added, %d removed, %d changed\n", len(changes.Added), len(changes.Removed), len(changes.Changed))
	if changes.IDsUnstable {
		fmt.Fprintln(w, "Warning: the two versions share too few identifiers for these lists to be an edit history (the publisher may have renumbered). Use --json for the full lists and compare by title.")
	}
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

			// The VERSION column appears only when some catalog is under a
			// document, so a list of plain catalogs renders unchanged.
			versioned := false
			for _, cat := range catalogs {
				versioned = versioned || cat.GetDocumentSlug() != ""
			}

			return emit(cmd,
				func(w io.Writer, color bool) {
					tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
					header := "ID\tNAME\tCONTROLS\tFORMAT"
					if versioned {
						header += "\tVERSION"
					}
					fmt.Fprintln(tw, header)
					for _, cat := range catalogs {
						format := cat.GetFormat().String()
						switch format {
						case "CATALOG_FORMAT_OSCAL":
							format = "OSCAL"
						case "CATALOG_FORMAT_GEMARA":
							format = "GEMARA"
						}
						row := fmt.Sprintf("%s\t%s\t%d\t%s",
							cat.GetCatalogId(),
							cat.GetName(),
							cat.GetControlCount(),
							format,
						)
						if versioned {
							version := terminalSafe(catalogversion.OfCatalog(cat))
							if cat.GetCurrent() {
								version += " (current)"
							}
							row += "\t" + version
						}
						fmt.Fprintln(tw, row)
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
					if hash := cat.GetProvenance().GetLineageMetadata()["content_hash"]; hash != "" {
						fmt.Fprintf(w, "Content SHA-256: %s\n", terminalSafe(hash))
					}
					if version := catalogversion.OfCatalog(cat); version != "" {
						fmt.Fprintf(w, "Document version: %s\n", terminalSafe(version))
						fmt.Fprintf(w, "Current: %t\n", cat.GetCurrent())
						if id := cat.GetSupersedesCatalogId(); id != "" {
							fmt.Fprintf(w, "Supersedes: %s\n", terminalSafe(id))
						}
					}
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

// newCatalogControlsCmd lists a catalog's controls as analysis receives
// them, so a reviewer sees the exact Title and Statement each analyzer is
// given.
func newCatalogControlsCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "controls <catalog-id>",
		Short: "List a catalog's controls as analysis receives them",
		Long: `List every control of an imported catalog with the title and statement
analysis gives each analyzer: a statement part's statement carries its
parents' lead-ins, a title that only repeats the control's label is dropped,
and a title-only control's title becomes its statement.

The text comes from the catalog and is untrusted. The text output shows each
field on one line, with control characters and invisible format characters
removed; --json gives the text unchanged, JSON-escaped. Do not render either
as markup (HTML, Markdown).

The whole catalog is read in one response; it is not paginated.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			catalogID := args[0]
			tenant, _ := cmd.Flags().GetString("tenant")

			req := &pb.ListCatalogControlsRequest{
				CatalogId: catalogID,
			}
			if tenant != "" {
				req.TenantContext = &pb.TenantContext{
					TenantId: tenant,
				}
			}

			resp, err := state.client.ListCatalogControls(cmd.Context(), connectrpc.NewRequest(req))
			if err != nil {
				return fmt.Errorf("failed to list catalog controls: %w", err)
			}

			controls := resp.Msg.GetControls()
			jsonControls := make([]map[string]any, len(controls))
			for i, c := range controls {
				jsonControls[i] = map[string]any{
					"control_id": c.GetControlId(),
					"title":      c.GetTitle(),
					"statement":  c.GetStatement(),
				}
			}

			return emit(cmd,
				func(w io.Writer, _ bool) {
					// Untrusted text is shown whole: cutting a statement
					// would misstate what analysis receives.
					for i, c := range controls {
						if i > 0 {
							fmt.Fprintln(w)
						}
						title := intanalyzer.CleanDisplay(c.GetTitle(), math.MaxInt)
						if title == "" {
							title = "(none)"
						}
						fmt.Fprintf(w, "Control: %s\n", intanalyzer.CleanDisplay(c.GetControlId(), math.MaxInt))
						fmt.Fprintf(w, "Title: %s\n", title)
						fmt.Fprintf(w, "Statement: %s\n", intanalyzer.CleanDisplay(c.GetStatement(), math.MaxInt))
					}
				},
				map[string]any{
					"catalog_id": catalogID,
					"controls":   jsonControls,
				},
			)
		},
	}

	guidedArgs(cmd, cobra.ExactArgs(1), argGuide{
		noun:  "a catalog ID",
		find:  "crosscodex catalog list",
		usage: "crosscodex catalog controls <catalog-id>",
		examples: []string{
			"crosscodex catalog controls cat_01234567890",
			"crosscodex catalog controls cat_01234567890 --json",
			"crosscodex catalog controls cat_01234567890 --tenant acme",
		},
	})

	cmd.Flags().String("tenant", "", "Tenant to act on; must be the tenant your client certificate authenticates (default: that tenant)")

	return cmd
}

func newCatalogRetireCmd(state *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "retire <slug>@<label>...",
		Short: "Retire old versions of a catalog document (admin only)",
		Long: `Retire versions of a catalog document: each version is archived, then
deleted with its controls, embeddings and cached results. The version that
superseded a retired one is linked to the retired version's predecessor, so
the document's history stays one chain. Graph nodes are left in place.

Name each version as <slug>@<label>, as "catalog list" shows them (split at
the first @). Several versions of one document are retired together: if any
of them is protected, none is retired and the error names every blocker. A
version is protected while it is the document's current version, a legal
hold covers it, a job under legal hold uses it, or a job that uses it has
attestations.

Retiring cannot be undone from the CLI: the archive copy is the only copy
left, and crosscodex has no command to restore it. Pass --yes to confirm;
without it the command refuses and names the versions it would retire.

Retiring needs the admin role, and a crosscodexd server running role all with
database.purge_dsn and retention.archive configured.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			slug, labels, err := parseRetireArgs(args)
			if err != nil {
				return err
			}
			if yes, _ := cmd.Flags().GetBool("yes"); !yes {
				versions := make([]string, len(labels))
				for i, label := range labels {
					versions[i] = fmt.Sprintf("%s@%q", slug, label)
				}
				return fmt.Errorf("refusing to retire %s without confirmation: retiring deletes each version with its controls, embeddings and cached results, and crosscodex has no command to restore it from the archive; pass --yes to confirm, or run \"crosscodex catalog list\" to review the versions first", strings.Join(versions, ", "))
			}
			resp, err := state.client.RetireCatalogVersion(cmd.Context(), connectrpc.NewRequest(&pb.RetireCatalogVersionRequest{
				DocumentSlug: slug,
				VersionLabel: labels,
			}))
			if err != nil {
				return fmt.Errorf("failed to retire catalog versions: %w", err)
			}
			ids := resp.Msg.GetRetiredCatalogIds()
			if len(ids) != len(labels) {
				return fmt.Errorf("the server retired %d versions for %d labels; run \"crosscodex catalog list\" to see which versions of %s remain", len(ids), len(labels), slug)
			}
			retired := make([]map[string]any, len(ids))
			for i, id := range ids {
				retired[i] = map[string]any{"version_label": labels[i], "catalog_id": id}
			}
			return emit(cmd,
				func(w io.Writer, color bool) {
					for i, id := range ids {
						fmt.Fprintf(w, "Retired %s @ %s (catalog %s)\n", slug, terminalSafe(labels[i]), terminalSafe(id))
					}
				},
				map[string]any{"document_slug": slug, "retired": retired},
			)
		},
	}

	guidedArgs(cmd, cobra.MinimumNArgs(1), argGuide{
		noun:  "one or more document versions as <slug>@<label>",
		find:  "crosscodex catalog list",
		usage: "crosscodex catalog retire <slug>@<label>...",
		examples: []string{
			`crosscodex catalog retire --yes "nist-800-53@Rev 4"`,
			`crosscodex catalog retire --yes "nist-800-53@Rev 3" "nist-800-53@Rev 4"`,
		},
	})

	cmd.Flags().BoolP("yes", "y", false, "Confirm that the named versions are deleted (required)")

	return cmd
}

// parseRetireArgs splits each <slug>@<label> argument at its first '@' and
// returns the one slug they share and their labels, repeats dropped.
func parseRetireArgs(args []string) (string, []string, error) {
	var slug string
	var labels []string
	seen := map[string]bool{}
	for _, arg := range args {
		s, label, ok := strings.Cut(arg, "@")
		if !ok {
			return "", nil, fmt.Errorf("%q is not <slug>@<label>; name the version as the document slug, '@' and the version label, for example \"nist-800-53@Rev 4\"", arg)
		}
		if err := catalog.ValidateDocumentSlug(s); err != nil {
			return "", nil, fmt.Errorf("%q: %w", arg, err)
		}
		if err := catalog.ValidateVersionLabel(label); err != nil {
			return "", nil, fmt.Errorf("%q: %w", arg, err)
		}
		if slug != "" && s != slug {
			return "", nil, fmt.Errorf("%q and %q name different documents; retire versions of one document per command, so that each command retires all of its versions or none", slug+"@"+labels[0], arg)
		}
		slug = s
		if !seen[label] {
			seen[label] = true
			labels = append(labels, label)
		}
	}
	return slug, labels, nil
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
	out := map[string]any{
		"catalog_id":    cat.GetCatalogId(),
		"name":          cat.GetName(),
		"format":        cat.GetFormat().String(),
		"version":       cat.GetVersion(),
		"control_count": cat.GetControlCount(),
		"properties":    cat.GetProperties(),
		// The catalog's content identity: the hex SHA-256 of the imported bytes
		// for OSCAL, the ingestion content hash for a converted document
		// (recompute with `crosscodex catalog content-hash`); "" when none was
		// recorded.
		"content_hash": cat.GetProvenance().GetLineageMetadata()["content_hash"],
	}
	// Document fields appear only for a catalog under a document, so
	// unversioned catalogs keep their previous JSON shape.
	if cat.GetDocumentSlug() != "" {
		out["document_slug"] = cat.GetDocumentSlug()
		out["version_label"] = cat.GetVersionLabel()
		out["version_display"] = catalogversion.OfCatalog(cat)
		out["supersedes_catalog_id"] = cat.GetSupersedesCatalogId()
		out["link_method"] = cat.GetLinkMethod()
		out["current"] = cat.GetCurrent()
	}
	return out
}

func catalogsToJSON(catalogs []*pb.Catalog) []map[string]any {
	result := make([]map[string]any, len(catalogs))
	for i, cat := range catalogs {
		result[i] = catalogToJSON(cat)
	}
	return result
}

func newCatalogContentHashCmd(_ *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "content-hash <file>",
		Short: "Print the content hash an import of a file records",
		Long: `Print the content hash that importing a file records for its catalog.

The result is the content_hash that 'catalog inspect' shows for the catalog
imported from this file. For crosscodex-ingestion output it is the hash a
daemon with ingestion configured records; it ignores timing and other fields
crosscodex does not read. For anything else it is the file's SHA-256.

Run it on the converted output (structured.json), not on an uploaded PDF,
DOCX or HTML source: a source file reports kind "bytes", which never equals
the catalog's content_hash. The source file's SHA-256 is the ingest link's
content_hash byproduct instead.

With --json the keys are path, kind ("ingestion" or "bytes") and
content_hash. Malformed ingestion output exits with the adapter's error;
re-run the conversion.

This command runs locally and does not require a running daemon.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			filePath := args[0]
			content, err := os.ReadFile(filePath)
			if err != nil {
				return fmt.Errorf("failed to read file: %w", err)
			}

			kind, hash, err := catalog.ContentIdentity(content)
			if err != nil {
				return err
			}

			return emit(cmd,
				func(w io.Writer, color bool) {
					fmt.Fprintf(w, "content_hash: %s (%s)\n", hash, kind)
					fmt.Fprintf(w, "  Path: %s\n", filePath)
				},
				map[string]string{
					"path":         filePath,
					"kind":         kind,
					"content_hash": hash,
				},
			)
		},
	}

	guidedArgs(cmd, cobra.ExactArgs(1), argGuide{
		noun:  "a file path",
		find:  "crosscodex catalog content-hash <file>",
		usage: "crosscodex catalog content-hash <file>",
		examples: []string{
			"crosscodex catalog content-hash structured.json",
			"crosscodex catalog content-hash oscal-catalog.json",
		},
	})

	return cmd
}
