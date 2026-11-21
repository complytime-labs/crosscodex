package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"

	connectrpc "connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
)

// retireGateway answers RetireCatalogVersion with a catalog ID per label, or
// with err; every other RPC is unimplemented.
type retireGateway struct {
	crosscodexv1connect.UnimplementedGatewayServiceHandler
	err      error
	requests []*pb.RetireCatalogVersionRequest
}

func (g *retireGateway) RetireCatalogVersion(_ context.Context, req *connectrpc.Request[pb.RetireCatalogVersionRequest]) (*connectrpc.Response[pb.RetireCatalogVersionResponse], error) {
	g.requests = append(g.requests, req.Msg)
	if g.err != nil {
		return nil, g.err
	}
	ids := make([]string, len(req.Msg.GetVersionLabel()))
	for i := range ids {
		ids[i] = "cat-" + req.Msg.GetVersionLabel()[i]
	}
	return connectrpc.NewResponse(&pb.RetireCatalogVersionResponse{RetiredCatalogIds: ids}), nil
}

var _ = Describe("catalog retire", func() {
	run := func(gw *retireGateway, args ...string) (string, error) {
		path, handler := crosscodexv1connect.NewGatewayServiceHandler(gw)
		mux := http.NewServeMux()
		mux.Handle(path, handler)
		srv := httptest.NewServer(mux)
		DeferCleanup(srv.Close)

		root := &cobra.Command{Use: "crosscodex"}
		root.PersistentFlags().Bool("json", false, "")
		root.PersistentFlags().Bool("plain", false, "")
		root.PersistentFlags().Bool("no-color", false, "")
		root.AddCommand(newCatalogCmd(&cliState{client: crosscodexv1connect.NewGatewayServiceClient(srv.Client(), srv.URL)}))
		stdout, _, err := execute(root, append([]string{"catalog", "retire"}, args...)...)
		return stdout, err
	}

	It("retires every named version of one document in one request and reports each", func() {
		gw := &retireGateway{}
		out, err := run(gw, "nist@Rev 3", "nist@Rev 4", "nist@Rev 3", "--plain")
		Expect(err).NotTo(HaveOccurred())
		Expect(gw.requests).To(HaveLen(1))
		Expect(gw.requests[0].GetDocumentSlug()).To(Equal("nist"))
		Expect(gw.requests[0].GetVersionLabel()).To(Equal([]string{"Rev 3", "Rev 4"}))
		Expect(out).To(Equal("Retired nist @ Rev 3 (catalog cat-Rev 3)\nRetired nist @ Rev 4 (catalog cat-Rev 4)\n"))
	})

	It("splits at the first @, so a label may contain one", func() {
		gw := &retireGateway{}
		_, err := run(gw, "nist@v1@draft")
		Expect(err).NotTo(HaveOccurred())
		Expect(gw.requests[0].GetVersionLabel()).To(Equal([]string{"v1@draft"}))
	})

	It("prints the retired versions as JSON", func() {
		out, err := run(&retireGateway{}, "nist@Rev 4", "--json")
		Expect(err).NotTo(HaveOccurred())
		var got map[string]any
		Expect(json.Unmarshal([]byte(out), &got)).To(Succeed())
		Expect(got).To(Equal(map[string]any{
			"document_slug": "nist",
			"retired":       []any{map[string]any{"version_label": "Rev 4", "catalog_id": "cat-Rev 4"}},
		}))
	})

	DescribeTable("refuses arguments it cannot send, before calling the server",
		func(args []string, wantMsg string) {
			gw := &retireGateway{}
			_, err := run(gw, args...)
			Expect(err).To(MatchError(ContainSubstring(wantMsg)))
			Expect(gw.requests).To(BeEmpty())
		},
		Entry("no argument", []string{}, "requires one or more document versions"),
		Entry("no @", []string{"nist"}, `"nist" is not <slug>@<label>`),
		Entry("an empty label", []string{"nist@"}, "invalid version label"),
		Entry("an invalid slug", []string{"NIST@Rev 4"}, "invalid document slug"),
		Entry("two documents", []string{"nist@Rev 4", "iso@2022"}, "retire versions of one document per command"),
	)

	It("returns the server's refusal", func() {
		gw := &retireGateway{err: connectrpc.NewError(connectrpc.CodeFailedPrecondition,
			errors.New(`catalog version is protected: legal hold "litigation" covers nist@"Rev 4"`))}
		_, err := run(gw, "nist@Rev 4")
		Expect(err).To(MatchError(ContainSubstring(`failed to retire catalog versions: failed_precondition: catalog version is protected: legal hold "litigation"`)))
	})
})
