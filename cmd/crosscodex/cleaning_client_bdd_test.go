//go:build !integration

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"

	connectrpc "connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
)

type listFindingsStub struct {
	crosscodexv1connect.UnimplementedEdgeCleaningServiceHandler
}

func (listFindingsStub) ListFindings(context.Context, *connectrpc.Request[pb.ListFindingsRequest]) (*connectrpc.Response[pb.ListFindingsResponse], error) {
	return connectrpc.NewResponse(&pb.ListFindingsResponse{}), nil
}

// Every client constructor yields an EdgeCleaningService client on the same
// base URL and transport as the gateway client.
var _ = Describe("EdgeCleaningService client construction", func() {
	var server *httptest.Server

	BeforeEach(func() {
		mux := http.NewServeMux()
		mux.Handle(crosscodexv1connect.NewEdgeCleaningServiceHandler(listFindingsStub{}))
		server = httptest.NewServer(mux)
		DeferCleanup(server.Close)
	})

	list := func(client crosscodexv1connect.EdgeCleaningServiceClient) error {
		Expect(client).NotTo(BeNil())
		_, err := client.ListFindings(context.Background(), connectrpc.NewRequest(&pb.ListFindingsRequest{}))
		return err
	}

	It("connectClient reaches the cleaning service over plain HTTP", func() {
		_, _, cleaningClient := connectClient(strings.TrimPrefix(server.URL, "http://"))
		Expect(list(cleaningClient)).To(Succeed())
	})

	It("newTLSGatewayClient reaches the cleaning service through the given transport", func() {
		tlsServer := httptest.NewTLSServer(server.Config.Handler)
		DeferCleanup(tlsServer.Close)

		_, _, cleaningClient := newTLSGatewayClient(strings.TrimPrefix(tlsServer.URL, "https://"), tlsServer.Client())
		Expect(list(cleaningClient)).To(Succeed())
	})

	It("reports an unmounted cleaning service as an error, as the embedded daemon's missing AdminService is", func() {
		bare := httptest.NewServer(http.NewServeMux())
		DeferCleanup(bare.Close)

		_, _, cleaningClient := connectClient(strings.TrimPrefix(bare.URL, "http://"))
		Expect(connectrpc.CodeOf(list(cleaningClient))).To(Equal(connectrpc.CodeUnimplemented))
	})
})
