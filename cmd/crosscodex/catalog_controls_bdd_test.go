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

// controlsGateway answers ListCatalogControls with controls, or err when
// set, and records the last request; every other RPC is unimplemented.
type controlsGateway struct {
	crosscodexv1connect.UnimplementedGatewayServiceHandler
	controls []*pb.Control
	err      error
	got      *pb.ListCatalogControlsRequest
}

func (g *controlsGateway) ListCatalogControls(_ context.Context, req *connectrpc.Request[pb.ListCatalogControlsRequest]) (*connectrpc.Response[pb.ListCatalogControlsResponse], error) {
	g.got = req.Msg
	if g.err != nil {
		return nil, g.err
	}
	return connectrpc.NewResponse(&pb.ListCatalogControlsResponse{Controls: g.controls}), nil
}

var _ = Describe("catalog controls", func() {
	run := func(gw *controlsGateway, args ...string) (string, error) {
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
		stdout, _, err := execute(root, append([]string{"catalog", "controls"}, args...)...)
		return stdout, err
	}

	hostile := []*pb.Control{
		{ControlId: "cat-1/ac-2", CatalogId: "cat-1", Title: "Account Management", Statement: "The organization:\nSpecify:"},
		{ControlId: "cat-1/ac-2.d.1\x1b[31m", CatalogId: "cat-1", Title: "", Statement: "Authorized\u202e users;"},
	}

	It("prints each control's ID, title and statement, cleaned to one line each", func() {
		gw := &controlsGateway{controls: hostile}
		out, err := run(gw, "cat-1", "--plain")
		Expect(err).NotTo(HaveOccurred())

		Expect(gw.got.GetCatalogId()).To(Equal("cat-1"))
		Expect(gw.got.GetTenantContext()).To(BeNil())
		Expect(out).To(Equal(
			"Control: cat-1/ac-2\nTitle: Account Management\nStatement: The organization: Specify:\n\n" +
				"Control: cat-1/ac-2.d.1 [31m\nTitle: (none)\nStatement: Authorized users;\n"))
	})

	It("emits the catalog ID and each control's raw text as JSON with --json", func() {
		out, err := run(&controlsGateway{controls: hostile}, "cat-1", "--json")
		Expect(err).NotTo(HaveOccurred())

		var got struct {
			CatalogID string `json:"catalog_id"`
			Controls  []struct {
				ControlID string `json:"control_id"`
				Title     string `json:"title"`
				Statement string `json:"statement"`
			} `json:"controls"`
		}
		Expect(json.Unmarshal([]byte(out), &got)).To(Succeed())
		Expect(got.CatalogID).To(Equal("cat-1"))
		Expect(got.Controls).To(HaveLen(2))
		Expect(got.Controls[0].Statement).To(Equal("The organization:\nSpecify:"))
		Expect(got.Controls[1].ControlID).To(Equal("cat-1/ac-2.d.1\x1b[31m"))
		Expect(got.Controls[1].Title).To(BeEmpty())
		Expect(got.Controls[1].Statement).To(Equal("Authorized\u202e users;"))
	})

	It("emits an empty controls array, not null, for a catalog without controls", func() {
		out, err := run(&controlsGateway{}, "cat-1", "--json")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring(`"controls": []`))
	})

	It("sends --tenant as the request's tenant_context", func() {
		gw := &controlsGateway{}
		_, err := run(gw, "cat-1", "--tenant", "acme")
		Expect(err).NotTo(HaveOccurred())
		Expect(gw.got.GetTenantContext().GetTenantId()).To(Equal("acme"))
	})

	It("returns the gateway's NotFound with its remedy", func() {
		_, err := run(&controlsGateway{err: connectrpc.NewError(connectrpc.CodeNotFound,
			errors.New(`catalog "cat-9" is not a catalog of your tenant. Run "crosscodex catalog list"`))}, "cat-9")
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeNotFound))
		Expect(err.Error()).To(ContainSubstring("crosscodex catalog list"))
	})

	It("refuses a missing catalog ID and names the command that lists them", func() {
		_, err := run(&controlsGateway{})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("crosscodex catalog list"))
	})
})
