//go:build integration

package main

import (
	"context"
	"io"
	"log/slog"
	"os"

	connectrpc "connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"go.opentelemetry.io/otel"
)

// This spec proves buildEmbeddedService (the production wiring in
// embedded.go) hands the active global otel providers to the services it
// constructs, rather than leaving them on their no-op defaults.
//
// It cannot reuse sharedState/startTestDaemon from
// embedded_integration_suite_test.go: that fixture boots the daemon through
// startEmbeddedDaemon, which calls telemetry.Init before buildEmbeddedService.
// telemetry.Init unconditionally calls otel.SetTracerProvider /
// otel.SetMeterProvider — with no Observability config it installs no-op
// providers, clobbering any telemetrytest.TestProvider installed beforehand.
// So this spec calls buildEmbeddedService directly, mirroring how
// cmd/crosscodexd/telemetry_wiring_bdd_test.go calls bootstrap directly
// instead of going through main's telemetry.Init.
var _ = Describe("embedded service telemetry wiring", func() {
	var (
		ctx context.Context
		tp  *telemetrytest.TestProvider
	)

	BeforeEach(func() {
		ctx = context.Background()
		DeferCleanup(testspecs.IsolateTelemetryGlobals())

		var err error
		tp, err = telemetrytest.NewTestProvider()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(tp.Shutdown, context.Background())
		otel.SetTracerProvider(tp.TracerProvider())
		otel.SetMeterProvider(tp.MeterProvider())
	})

	It("instruments the gateway, catalog, and db pool it builds for embedded mode", func() {
		dataDir, err := os.MkdirTemp("", "embedded-telemetry-data-")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(os.RemoveAll, dataDir)
		GinkgoT().Setenv("XDG_DATA_HOME", dataDir)

		cfg := &config.Config{}
		cfg.Database.DSN = suDSN

		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		svc, resources, err := buildEmbeddedService(ctx, cfg, logger)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(resources.close)

		// Mirror the gateway's connectAuthInterceptor: inject the identity
		// and tenant context it would set after a successful mTLS handshake,
		// since we are calling the Service methods directly rather than
		// through the Connect HTTP server.
		identity := &authn.Identity{
			Subject:  "telemetry-wiring-test",
			TenantID: embeddedTenantID,
			Method:   authn.AuthMethodMTLS,
		}
		callCtx := authn.WithIdentity(ctx, identity)
		callCtx, err = tenant.WithTenant(callCtx, identity.TenantID)
		Expect(err).NotTo(HaveOccurred())
		callCtx = tenant.WithUser(callCtx, identity.Subject)

		data := fixture("minimal-catalog.json")
		submitResp, err := svc.SubmitDocument(callCtx, connectrpc.NewRequest(&pb.SubmitDocumentRequest{
			Source:        &pb.SubmitDocumentRequest_Content{Content: data},
			CatalogFormat: pb.CatalogFormat_CATALOG_FORMAT_OSCAL,
			CatalogName:   "telemetry-wiring",
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(submitResp.Msg.GetDocumentId()).NotTo(BeEmpty())
		Expect(submitResp.Msg.GetJobId()).NotTo(BeEmpty())

		listResp, err := svc.ListCatalogs(callCtx, connectrpc.NewRequest(&pb.ListCatalogsRequest{}))
		Expect(err).NotTo(HaveOccurred())
		Expect(listResp.Msg.GetCatalogs()).NotTo(BeEmpty())

		Eventually(func() []string {
			scopes := map[string]bool{}
			for _, s := range tp.GetSpans() {
				scopes[s.InstrumentationScope().Name] = true
			}
			names := make([]string, 0, len(scopes))
			for name := range scopes {
				names = append(names, name)
			}
			return names
		}).Should(SatisfyAll(
			// internal/gateway and internal/catalog emit no span at all
			// without a tracer, so their presence discriminates passing the
			// telemetry option from leaving it unset.
			ContainElement("crosscodex/internal/gateway"),
			ContainElement("crosscodex/internal/catalog"),
			ContainElement("crosscodex/pkg/db"),
		))

		var dbSpanNames []string
		for _, s := range tp.GetSpans() {
			if s.InstrumentationScope().Name == "crosscodex/pkg/db" {
				dbSpanNames = append(dbSpanNames, s.Name())
			}
		}
		Expect(dbSpanNames).NotTo(BeEmpty())
		Expect(dbSpanNames).To(ContainElement(BeElementOf("db.Begin", "db.Query", "db.QueryRow", "db.Exec")),
			"expected a span from the pool's own tracer; the migrator and tenant pool also emit under crosscodex/pkg/db via the global provider")
	})
})
