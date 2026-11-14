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

	connectrpc "connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
)

// importGateway answers StreamDocument with a fixed response; every other
// RPC is unimplemented.
type importGateway struct {
	crosscodexv1connect.UnimplementedGatewayServiceHandler
	resp *pb.SubmitDocumentResponse
}

func (g *importGateway) StreamDocument(_ context.Context, stream *connectrpc.ClientStream[pb.StreamDocumentChunk]) (*connectrpc.Response[pb.SubmitDocumentResponse], error) {
	for stream.Receive() {
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
