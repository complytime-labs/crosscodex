package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	connectrpc "connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
)

// importGateway answers StreamDocument with a fixed response; every other
// RPC is unimplemented.
type importGateway struct {
	crosscodexv1connect.UnimplementedGatewayServiceHandler
	resp *pb.SubmitDocumentResponse
	// metadata records the first message of the last stream.
	metadata *pb.StreamDocumentMetadata
	// content records the chunks of the last stream, concatenated.
	content []byte
	// catalogs answers ListCatalogs.
	catalogs []*pb.Catalog
}

func (g *importGateway) GetCatalog(context.Context, *connectrpc.Request[pb.GetCatalogRequest]) (*connectrpc.Response[pb.GetCatalogResponse], error) {
	return connectrpc.NewResponse(&pb.GetCatalogResponse{Catalog: g.catalogs[0]}), nil
}

func (g *importGateway) ListCatalogs(context.Context, *connectrpc.Request[pb.ListCatalogsRequest]) (*connectrpc.Response[pb.ListCatalogsResponse], error) {
	return connectrpc.NewResponse(&pb.ListCatalogsResponse{Catalogs: g.catalogs}), nil
}

func (g *importGateway) StreamDocument(_ context.Context, stream *connectrpc.ClientStream[pb.StreamDocumentChunk]) (*connectrpc.Response[pb.SubmitDocumentResponse], error) {
	for stream.Receive() {
		if m := stream.Msg().GetMetadata(); m != nil {
			g.metadata = m
		}
		g.content = append(g.content, stream.Msg().GetChunk()...)
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return connectrpc.NewResponse(g.resp), nil
}

func textlessControls(n int, partNames ...string) []*pb.TextlessControl {
	out := make([]*pb.TextlessControl, n)
	for i := range out {
		out[i] = &pb.TextlessControl{Identifier: fmt.Sprintf("C-%d", i+1), PartNames: partNames}
	}
	return out
}

var _ = Describe("catalog import reporting", func() {
	run := func(resp *pb.SubmitDocumentResponse, extra ...string) string {
		path, handler := crosscodexv1connect.NewGatewayServiceHandler(&importGateway{resp: resp})
		mux := http.NewServeMux()
		mux.Handle(path, handler)
		srv := httptest.NewServer(mux)
		DeferCleanup(srv.Close)

		file := filepath.Join(GinkgoT().TempDir(), "cat.json")
		Expect(os.WriteFile(file, []byte(`{}`), 0o600)).To(Succeed())

		root := &cobra.Command{Use: "crosscodex"}
		root.PersistentFlags().Bool("json", false, "")
		root.PersistentFlags().Bool("plain", false, "")
		root.PersistentFlags().Bool("no-color", false, "")
		root.AddCommand(newCatalogCmd(&cliState{client: crosscodexv1connect.NewGatewayServiceClient(srv.Client(), srv.URL)}))
		stdout, _, err := execute(root, append([]string{"catalog", "import", file}, extra...)...)
		Expect(err).NotTo(HaveOccurred())
		return stdout
	}

	It("names the first ten textless controls, the rest as a count, and the fix", func() {
		out := run(&pb.SubmitDocumentResponse{
			DocumentId: "doc-1", CatalogId: "cat-1",
			TextlessControls: textlessControls(12, "overview", "guidance"),
		}, "--plain")
		Expect(out).To(ContainSubstring("12 controls have no requirement text and will not be analysed: " +
			"C-1, C-2, C-3, C-4, C-5, C-6, C-7, C-8, C-9, C-10, and 2 more.\n"))
		Expect(out).To(ContainSubstring("Their parts are named: guidance, overview. " +
			"If one of these holds the requirement, add it to catalog.requirement_part_names and re-import.\n"))
	})

	It("lists every identifier without a remainder when there are ten or fewer", func() {
		out := run(&pb.SubmitDocumentResponse{
			DocumentId: "doc-1", CatalogId: "cat-1",
			TextlessControls: textlessControls(2, "overview"),
		}, "--plain")
		Expect(out).To(ContainSubstring("2 controls have no requirement text and will not be analysed: C-1, C-2.\n"))
		Expect(out).NotTo(ContainSubstring("more"))
	})

	It("says nothing about parts when no textless control has any", func() {
		out := run(&pb.SubmitDocumentResponse{
			DocumentId: "doc-1", CatalogId: "cat-1",
			TextlessControls: textlessControls(1),
		}, "--plain")
		Expect(out).To(ContainSubstring("1 control has no requirement text and will not be analysed: C-1.\n"))
		Expect(out).NotTo(ContainSubstring("Their parts are named"))
	})

	It("escapes control characters in catalog-derived identifiers and part names", func() {
		out := run(&pb.SubmitDocumentResponse{
			DocumentId: "doc-1", CatalogId: "cat-1",
			TextlessControls: []*pb.TextlessControl{{Identifier: "AC-1\x1b[31m", PartNames: []string{"over\nview"}}},
		}, "--plain")
		Expect(out).NotTo(ContainSubstring("\x1b"))
		Expect(out).To(ContainSubstring(`AC-1\x1b[31m.`))
		Expect(out).To(ContainSubstring(`Their parts are named: over\nview. If one`))
		Expect(strings.Split(strings.TrimSuffix(out, "\n"), "\n")).To(HaveLen(6), "no injected line break")
	})

	DescribeTable("escapes non-printable runes that unicode.IsControl misses and keeps printable text",
		func(identifier, want string) {
			out := run(&pb.SubmitDocumentResponse{
				DocumentId: "doc-1", CatalogId: "cat-1",
				TextlessControls: []*pb.TextlessControl{{Identifier: identifier}},
			}, "--plain")
			Expect(out).To(ContainSubstring("analysed: " + want + ".\n"))
		},
		Entry("a bidi override is escaped", "AC\u202e-1", `AC\u202e-1`),
		Entry("a zero-width space is escaped", "AC\u200b-1", `AC\u200b-1`),
		Entry("a C1 control is escaped", "AC\u0085-1", `AC\u0085-1`),
		Entry("a line separator is escaped", "AC\u2028-1", `AC\u2028-1`),
		Entry("a non-ASCII printable name is unchanged", "Übersicht 1", "Übersicht 1"),
	)

	It("prints no warning when every control has text", func() {
		out := run(&pb.SubmitDocumentResponse{DocumentId: "doc-1", CatalogId: "cat-1"}, "--plain")
		Expect(out).To(ContainSubstring("Catalog ID: cat-1"))
		Expect(out).NotTo(ContainSubstring("no requirement text"))
		Expect(out).NotTo(ContainSubstring("Their parts"))
	})

	It("emits the full list in JSON", func() {
		out := run(&pb.SubmitDocumentResponse{
			DocumentId: "doc-1", CatalogId: "cat-1",
			TextlessControls: textlessControls(12, "overview"),
		}, "--json")
		var got struct {
			CatalogID        string `json:"catalog_id"`
			DocumentID       string `json:"document_id"`
			TextlessControls []struct {
				Identifier string   `json:"identifier"`
				PartNames  []string `json:"part_names"`
			} `json:"textless_controls"`
		}
		Expect(json.Unmarshal([]byte(out), &got)).To(Succeed())
		Expect(got.CatalogID).To(Equal("cat-1"))
		Expect(got.DocumentID).To(Equal("doc-1"))
		Expect(got.TextlessControls).To(HaveLen(12))
		Expect(got.TextlessControls[11].Identifier).To(Equal("C-12"))
		Expect(got.TextlessControls[0].PartNames).To(Equal([]string{"overview"}))
	})

	It("emits an empty array, not null, when nothing is textless", func() {
		out := run(&pb.SubmitDocumentResponse{DocumentId: "doc-1", CatalogId: "cat-1"}, "--json")
		Expect(out).To(ContainSubstring(`"textless_controls": []`))
	})
})

// serveCatalogCmd starts gw and returns a runner for `catalog` CLI args
// against it; an import is given a file holding content as its argument.
func serveCatalogCmd(gw *importGateway, content []byte) func(args ...string) (string, error) {
	path, handler := crosscodexv1connect.NewGatewayServiceHandler(gw)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	DeferCleanup(srv.Close)

	file := filepath.Join(GinkgoT().TempDir(), "cat.json")
	Expect(os.WriteFile(file, content, 0o600)).To(Succeed())

	return func(args ...string) (string, error) {
		root := &cobra.Command{Use: "crosscodex"}
		root.PersistentFlags().Bool("json", false, "")
		root.PersistentFlags().Bool("plain", false, "")
		root.PersistentFlags().Bool("no-color", false, "")
		root.AddCommand(newCatalogCmd(&cliState{client: crosscodexv1connect.NewGatewayServiceClient(srv.Client(), srv.URL)}))
		full := append([]string{"catalog"}, args...)
		if full[1] == "import" {
			full = append(full[:2], append([]string{file}, full[2:]...)...)
		}
		stdout, _, err := execute(root, full...)
		return stdout, err
	}
}

var _ = Describe("catalog import format hint", func() {
	resp := &pb.SubmitDocumentResponse{DocumentId: "d", CatalogId: "c"}
	catalogJSON := []byte(`{"catalog":{"uuid":"u","metadata":{"title":"T"}}}`)

	DescribeTable("sends OSCAL only for OSCAL model JSON unless --raw, and the file unchanged",
		func(content []byte, want pb.CatalogFormat, args ...string) {
			gw := &importGateway{resp: resp}
			_, err := serveCatalogCmd(gw, content)(append([]string{"import", "--plain"}, args...)...)
			Expect(err).NotTo(HaveOccurred())
			Expect(gw.metadata.GetCatalogFormat()).To(Equal(want))
			Expect(gw.content).To(Equal(content))
		},
		Entry("OSCAL catalog JSON: OSCAL", catalogJSON, pb.CatalogFormat_CATALOG_FORMAT_OSCAL),
		Entry("OSCAL profile JSON: OSCAL, so the server explains it is not a catalog",
			[]byte(`{"profile":{"uuid":"u"}}`), pb.CatalogFormat_CATALOG_FORMAT_OSCAL),
		Entry("PDF bytes: unspecified, so the server detects and converts",
			[]byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n1 0 obj\n"), pb.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED),
		Entry("non-OSCAL JSON: unspecified", []byte(`{"elements":[]}`), pb.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED),
		Entry("--raw on OSCAL catalog JSON: unspecified", catalogJSON, pb.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED, "--raw"),
	)

	It("says in --help that documents need ingestion configured on the server", func() {
		out, err := serveCatalogCmd(&importGateway{resp: resp}, catalogJSON)("import", "--help")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("PDF, DOCX, HTML"))
		Expect(out).To(ContainSubstring("ingestion.address"))
		Expect(out).NotTo(ContainSubstring("Only OSCAL JSON is accepted"))
	})
})

var _ = Describe("catalog import of a structured source", func() {
	resp := &pb.SubmitDocumentResponse{DocumentId: "d", CatalogId: "c"}

	DescribeTable("sends --mapping to the server and lets it detect the format",
		func(content []byte, args ...string) {
			gw := &importGateway{resp: resp}
			_, err := serveCatalogCmd(gw, content)(append([]string{"import", "--plain", "--mapping", "owasp-asvs-5.0.0-json"}, args...)...)
			Expect(err).NotTo(HaveOccurred())
			Expect(gw.metadata.GetIngestionMapping()).To(Equal("owasp-asvs-5.0.0-json"))
			Expect(gw.metadata.GetCatalogFormat()).To(Equal(pb.CatalogFormat_CATALOG_FORMAT_UNSPECIFIED))
			Expect(gw.content).To(Equal(content))
		},
		Entry("structured JSON", []byte(`{"chapters":[]}`)),
		Entry("JSON that looks like OSCAL is still read through the mapping",
			[]byte(`{"catalog":{"uuid":"u","metadata":{"title":"T"}}}`)),
		Entry("with --document", []byte(`{"chapters":[]}`), "--document", "asvs"),
	)

	It("sends no mapping without --mapping", func() {
		gw := &importGateway{resp: resp}
		_, err := serveCatalogCmd(gw, []byte(`{}`))("import", "--plain")
		Expect(err).NotTo(HaveOccurred())
		Expect(gw.metadata.GetIngestionMapping()).To(BeEmpty())
	})

	DescribeTable("rejects a malformed --mapping before uploading, saying how to fix it",
		func(mapping string) {
			gw := &importGateway{resp: resp}
			_, err := serveCatalogCmd(gw, []byte(`{}`))("import", "--plain", "--mapping", mapping)
			Expect(err).To(MatchError(ContainSubstring("--mapping: invalid mapping")))
			Expect(err).To(MatchError(ContainSubstring("lowercase letters and digits")))
			Expect(gw.metadata).To(BeNil(), "nothing was sent")
		},
		Entry("uppercase", "OWASP-ASVS"),
		Entry("a file name with a slash", "maps/asvs"),
		Entry("too long", strings.Repeat("a", 129)),
	)

	It("explains --mapping in --help", func() {
		out, err := serveCatalogCmd(&importGateway{resp: resp}, []byte(`{}`))("import", "--help")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("--mapping"))
		Expect(out).To(ContainSubstring("JSON, CSV or XLSX"))
		Expect(out).To(ContainSubstring("registered on the ingestion server"))
		Expect(out).To(ContainSubstring("owasp-asvs-5.0.0-json"))
	})
})

var _ = Describe("catalog import under a document", func() {
	// serve starts a gateway fake and returns a runner for CLI args against it.
	serve := func(gw *importGateway) func(args ...string) (string, error) {
		return serveCatalogCmd(gw, []byte(`{}`))
	}

	versioned := &pb.SubmitDocumentResponse{
		DocumentId: "doc-2", CatalogId: "cat-2",
		Version:             "acme-sdlc @ 2026 rev B (2026-11-21 · abc123)",
		SupersededCatalogId: "cat-1",
		ChangeReport: &pb.ChangeReport{
			Added: []string{"N-1", "N-2"}, Removed: []string{"O-1"}, Changed: []string{"C-1", "C-2", "C-3"},
		},
	}

	It("sends the document slug and version label to the server", func() {
		gw := &importGateway{resp: versioned}
		run := serve(gw)
		_, err := run("import", "--plain", "--document", "acme-sdlc", "--version-label", "2026 rev B")
		Expect(err).NotTo(HaveOccurred())
		Expect(gw.metadata.GetDocumentSlug()).To(Equal("acme-sdlc"))
		Expect(gw.metadata.GetVersionLabel()).To(Equal("2026 rev B"))
	})

	It("prints the version, what it superseded and the change counts", func() {
		out, err := serve(&importGateway{resp: versioned})("import", "--plain", "--document", "acme-sdlc")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("Version: acme-sdlc @ 2026 rev B (2026-11-21 · abc123)\n"))
		Expect(out).To(ContainSubstring("Supersedes: cat-1\n"))
		Expect(out).To(ContainSubstring("Changes: 2 added, 1 removed, 3 changed\n"))
	})

	It("warns that identifiers are unstable instead of presenting an edit history", func() {
		resp := &pb.SubmitDocumentResponse{
			DocumentId: "d", CatalogId: "c", Version: "s @ l (d · h)", SupersededCatalogId: "p",
			ChangeReport: &pb.ChangeReport{Added: []string{"A"}, Removed: []string{"B"}, IdsUnstable: true},
		}
		out, err := serve(&importGateway{resp: resp})("import", "--plain", "--document", "s")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("share too few identifiers"))
	})

	It("prints no version lines for an unversioned import", func() {
		out, err := serve(&importGateway{resp: &pb.SubmitDocumentResponse{DocumentId: "d", CatalogId: "c"}})("import", "--plain")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).NotTo(ContainSubstring("Version:"))
		Expect(out).NotTo(ContainSubstring("Supersedes:"))
		Expect(out).NotTo(ContainSubstring("Changes:"))
	})

	It("prints no change line for the first version of a document", func() {
		resp := &pb.SubmitDocumentResponse{DocumentId: "d", CatalogId: "c", Version: "s @ l (d · h)"}
		out, err := serve(&importGateway{resp: resp})("import", "--plain", "--document", "s")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("Version: s @ l (d · h)\n"))
		Expect(out).NotTo(ContainSubstring("Changes:"))
	})

	It("emits version, superseded catalog and the change report in JSON", func() {
		out, err := serve(&importGateway{resp: versioned})("import", "--json", "--document", "acme-sdlc")
		Expect(err).NotTo(HaveOccurred())
		var got struct {
			VersionDisplay      string `json:"version_display"`
			SupersededCatalogID string `json:"superseded_catalog_id"`
			ChangeReport        *struct {
				Added       []string `json:"added"`
				Removed     []string `json:"removed"`
				Changed     []string `json:"changed"`
				IDsUnstable bool     `json:"ids_unstable"`
			} `json:"change_report"`
		}
		Expect(json.Unmarshal([]byte(out), &got)).To(Succeed())
		Expect(got.VersionDisplay).To(Equal("acme-sdlc @ 2026 rev B (2026-11-21 · abc123)"))
		Expect(out).NotTo(ContainSubstring(`"version":`), "version_display is the only version key")
		Expect(got.SupersededCatalogID).To(Equal("cat-1"))
		Expect(got.ChangeReport.Added).To(Equal([]string{"N-1", "N-2"}))
		Expect(got.ChangeReport.Removed).To(Equal([]string{"O-1"}))
		Expect(got.ChangeReport.Changed).To(Equal([]string{"C-1", "C-2", "C-3"}))
	})

	It("omits the change report from JSON when there is none", func() {
		out, err := serve(&importGateway{resp: &pb.SubmitDocumentResponse{DocumentId: "d", CatalogId: "c"}})("import", "--json")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).NotTo(ContainSubstring("change_report"))
	})

	DescribeTable("rejects bad input before uploading, saying how to fix it",
		func(wantInErr string, args ...string) {
			gw := &importGateway{resp: versioned}
			_, err := serve(gw)(append([]string{"import", "--plain"}, args...)...)
			Expect(err).To(MatchError(ContainSubstring(wantInErr)))
			Expect(gw.metadata).To(BeNil(), "nothing was sent")
		},
		Entry("an uppercase slug", "document slug", "--document", "Acme"),
		Entry("a slug with a space", "document slug", "--document", "acme sdlc"),
		Entry("a label with markup", "markup character", "--document", "acme", "--version-label", "<b>x</b>"),
		Entry("a label with a newline", "control character", "--document", "acme", "--version-label", "a\nb"),
		Entry("a label without a document", "--version-label requires --document", "--version-label", "x"),
	)

	It("documents both flags in --help", func() {
		out, err := serve(&importGateway{resp: versioned})("import", "--help")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("--document"))
		Expect(out).To(ContainSubstring("--version-label"))
	})

	Describe("list and inspect", func() {
		created := &pb.AuditMetadata{CreatedAt: timestamppb.New(time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC))}
		provenance := &pb.ProvenanceMetadata{LineageMetadata: map[string]string{"content_hash": "abcdef0123456789"}}
		cats := []*pb.Catalog{
			{CatalogId: "cat-v", Name: "SDLC", Format: pb.CatalogFormat_CATALOG_FORMAT_OSCAL, ControlCount: 3,
				DocumentSlug: "acme-sdlc", VersionLabel: "2026 rev B", Current: true, Audit: created, Provenance: provenance},
			{CatalogId: "cat-u", Name: "Plain", Format: pb.CatalogFormat_CATALOG_FORMAT_OSCAL, ControlCount: 1},
		}

		It("lists a versioned catalog in the display form and an unversioned one as before", func() {
			out, err := serve(&importGateway{catalogs: cats})("list", "--plain")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("acme-sdlc @ 2026 rev B (2026-11-21 · abcdef)"))
			Expect(out).To(MatchRegexp(`cat-u\s+Plain\s+1\s+OSCAL\s*\n`))
		})

		It("marks the current version of a document", func() {
			out, err := serve(&importGateway{catalogs: cats})("list", "--plain")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(MatchRegexp(`cat-v.*\(current\)`))
			Expect(out).NotTo(MatchRegexp(`cat-u.*current`))
		})

		It("escapes control characters in a slug or label from the server", func() {
			hostile := &pb.Catalog{CatalogId: "x", DocumentSlug: "a\x1b[31m", VersionLabel: "b\nc", Audit: created, Provenance: provenance}
			out, err := serve(&importGateway{catalogs: []*pb.Catalog{hostile}})("list", "--plain")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).NotTo(ContainSubstring("\x1b"))
			Expect(strings.Count(out, "\n")).To(Equal(2), "header and one row only")
		})

		It("inspects a versioned catalog with its version, currency and predecessor", func() {
			withPrev := &pb.Catalog{CatalogId: "cat-v", DocumentSlug: "acme-sdlc", VersionLabel: "rev B", SupersedesCatalogId: "cat-prev",
				Current: true, Audit: created, Provenance: provenance}
			out, err := serve(&importGateway{catalogs: []*pb.Catalog{withPrev}})("inspect", "--plain", "cat-v")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("Document version: acme-sdlc @ rev B (2026-11-21 · abcdef)\n"))
			Expect(out).To(ContainSubstring("Current: true\n"))
			Expect(out).To(ContainSubstring("Supersedes: cat-prev\n"))
		})

		It("inspects a versioned catalog in JSON with the same document keys as list", func() {
			full := &pb.Catalog{CatalogId: "cat-v", DocumentSlug: "acme-sdlc", VersionLabel: "rev B", SupersedesCatalogId: "cat-prev",
				LinkMethod: "declared", Current: true, Audit: created, Provenance: provenance}
			out, err := serve(&importGateway{catalogs: []*pb.Catalog{full}})("inspect", "--json", "cat-v")
			Expect(err).NotTo(HaveOccurred())
			var got map[string]any
			Expect(json.Unmarshal([]byte(out), &got)).To(Succeed())
			Expect(got).To(HaveKeyWithValue("version_display", "acme-sdlc @ rev B (2026-11-21 · abcdef)"))
			Expect(got).To(HaveKeyWithValue("supersedes_catalog_id", "cat-prev"))
			Expect(got).To(HaveKeyWithValue("link_method", "declared"))
		})

		It("inspects an unversioned catalog without document lines", func() {
			out, err := serve(&importGateway{catalogs: cats[1:]})("inspect", "--plain", "cat-u")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).NotTo(ContainSubstring("Document version"))
			Expect(out).NotTo(ContainSubstring("Current:"))
		})

		It("inspects a catalog with the SHA-256 of its imported bytes in JSON and text", func() {
			out, err := serve(&importGateway{catalogs: cats[:1]})("inspect", "--json", "cat-v")
			Expect(err).NotTo(HaveOccurred())
			var got map[string]any
			Expect(json.Unmarshal([]byte(out), &got)).To(Succeed())
			Expect(got).To(HaveKeyWithValue("content_hash", "abcdef0123456789"))

			out, err = serve(&importGateway{catalogs: cats[:1]})("inspect", "--plain", "cat-v")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("Content SHA-256: abcdef0123456789\n"))
		})

		It("inspects a catalog without a recorded hash as an empty content_hash and no text line", func() {
			out, err := serve(&importGateway{catalogs: cats[1:]})("inspect", "--json", "cat-u")
			Expect(err).NotTo(HaveOccurred())
			var got map[string]any
			Expect(json.Unmarshal([]byte(out), &got)).To(Succeed())
			Expect(got).To(HaveKeyWithValue("content_hash", ""))

			out, err = serve(&importGateway{catalogs: cats[1:]})("inspect", "--plain", "cat-u")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).NotTo(ContainSubstring("Content SHA-256"))
		})

		It("escapes control characters in a content hash from the server", func() {
			hostile := &pb.Catalog{CatalogId: "x", Provenance: &pb.ProvenanceMetadata{LineageMetadata: map[string]string{"content_hash": "ab\x1b[2Jcd"}}}
			out, err := serve(&importGateway{catalogs: []*pb.Catalog{hostile}})("inspect", "--plain", "x")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).NotTo(ContainSubstring("\x1b"))
		})

		It("carries document fields into the JSON list", func() {
			out, err := serve(&importGateway{catalogs: cats})("list", "--json")
			Expect(err).NotTo(HaveOccurred())
			var got []map[string]any
			Expect(json.Unmarshal([]byte(out), &got)).To(Succeed())
			Expect(got[0]).To(HaveKeyWithValue("document_slug", "acme-sdlc"))
			Expect(got[0]).To(HaveKeyWithValue("version_label", "2026 rev B"))
			Expect(got[0]).To(HaveKeyWithValue("current", true))
			Expect(got[0]).To(HaveKeyWithValue("version_display", "acme-sdlc @ 2026 rev B (2026-11-21 · abcdef)"))
			Expect(got[0]).To(HaveKeyWithValue("supersedes_catalog_id", ""))
			Expect(got[0]).To(HaveKeyWithValue("link_method", ""))
			Expect(got[1]).NotTo(HaveKey("document_slug"))
		})
	})
})
