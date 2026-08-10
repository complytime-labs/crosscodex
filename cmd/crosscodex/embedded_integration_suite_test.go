//go:build integration

package main

import (
	"context"
	"os"
	"path/filepath"

	connectrpc "connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/pkg/config"
	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
)

// This package's single Ginkgo bootstrap lives in main_bdd_test.go
// (TestMainBDD). Go compiles one test binary per package, and Ginkgo
// permits only one RunSpecs call per binary, so the embedded e2e specs
// below register into that same suite via the BeforeSuite/Describe blocks.

var (
	suDSN       string
	sharedState *cliState
	sharedStop  func()
)

var _ = BeforeSuite(func() {
	suDSN = os.Getenv("TEST_DATABASE_DSN")
	if suDSN == "" {
		Skip("TEST_DATABASE_DSN not set — run: task test:integration:embedded")
	}
	sharedState, sharedStop = startTestDaemon()
})

var _ = AfterSuite(func() {
	if sharedStop != nil {
		sharedStop()
	}
})

// startTestDaemon boots a real embedded daemon with isolated temp dirs.
func startTestDaemon() (*cliState, func()) {
	stateDir, err := os.MkdirTemp("", "embedded-e2e-state-")
	Expect(err).NotTo(HaveOccurred())
	dataDir, err := os.MkdirTemp("", "embedded-e2e-data-")
	Expect(err).NotTo(HaveOccurred())

	// Local blob storage lives under XDG_DATA_HOME; keep it in the temp dir.
	GinkgoT().Setenv("XDG_DATA_HOME", dataDir)

	cfg := &config.Config{}
	cfg.Database.DSN = suDSN

	state := &cliState{fullCfg: cfg}
	pidPath := filepath.Join(stateDir, "daemon.pid")

	err = startEmbeddedDaemon(context.Background(), state, stateDir, pidPath)
	Expect(err).NotTo(HaveOccurred(), "startEmbeddedDaemon should succeed against the compose DB")

	stop := func() {
		if state.daemon != nil {
			state.daemon.stop()
		}
		_ = os.RemoveAll(stateDir)
		_ = os.RemoveAll(dataDir)
	}
	return state, stop
}

func importCatalog(ctx context.Context, client crosscodexv1connect.GatewayServiceClient, data []byte, format pb.CatalogFormat, name string) (*pb.SubmitDocumentResponse, error) {
	stream := client.StreamDocument(ctx)
	sendErr := stream.Send(&pb.StreamDocumentChunk{
		Payload: &pb.StreamDocumentChunk_Metadata{
			Metadata: &pb.StreamDocumentMetadata{CatalogFormat: format, CatalogName: name},
		},
	})
	if sendErr == nil && len(data) > 0 {
		sendErr = stream.Send(&pb.StreamDocumentChunk{
			Payload: &pb.StreamDocumentChunk_Chunk{Chunk: data},
		})
	}
	resp, recvErr := stream.CloseAndReceive()
	if recvErr != nil {
		// The server processes after the stream closes, so the real RPC
		// error (InvalidArgument, Unimplemented, …) surfaces here.
		return nil, recvErr
	}
	if sendErr != nil {
		return nil, sendErr
	}
	return resp.Msg, nil
}

func truncateCatalogs(ctx context.Context, pool dbpkg.Pool) {
	Expect(pool.Exec(ctx, "TRUNCATE catalogs, controls CASCADE")).To(Succeed())
}

func fixture(name string) []byte {
	data, err := os.ReadFile(filepath.Join("testdata", "oscal", name))
	Expect(err).NotTo(HaveOccurred(), "read fixture %s", name)
	return data
}

func listCatalogs(ctx context.Context, client crosscodexv1connect.GatewayServiceClient) ([]*pb.Catalog, error) {
	resp, err := client.ListCatalogs(ctx, connectrpc.NewRequest(&pb.ListCatalogsRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetCatalogs(), nil
}

func getCatalog(ctx context.Context, client crosscodexv1connect.GatewayServiceClient, id string) (*pb.Catalog, error) {
	resp, err := client.GetCatalog(ctx, connectrpc.NewRequest(&pb.GetCatalogRequest{CatalogId: id}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetCatalog(), nil
}
