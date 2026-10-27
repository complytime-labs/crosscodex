//go:build !integration

package catalog_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/trace"

	crosscodexv1 "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/catalog"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
)

// failingStore fails every call with err, standing in for a database error.
type failingStore struct{ err error }

func (f failingStore) UpsertCatalog(context.Context, catalog.CatalogRecord) error { return f.err }
func (f failingStore) GetCatalog(context.Context, string) (*catalog.CatalogRecord, error) {
	return nil, f.err
}
func (f failingStore) ListCatalogs(context.Context, catalog.ListOptions) ([]catalog.CatalogRecord, catalog.PageInfo, error) {
	return nil, catalog.PageInfo{}, f.err
}
func (f failingStore) UpsertControls(context.Context, []catalog.ControlRecord) error { return f.err }
func (f failingStore) GetControl(context.Context, string) (*catalog.ControlRecord, error) {
	return nil, f.err
}
func (f failingStore) SearchControls(context.Context, catalog.SearchQuery) ([]catalog.ControlRecord, catalog.PageInfo, error) {
	return nil, catalog.PageInfo{}, f.err
}

var _ = Describe("RPC error messages", func() {
	// dbDetail stands in for what the store returns from PostgreSQL: SQL, a
	// SQLSTATE and a table name, none of which may reach a client.
	const dbDetail = `ERROR: new row violates row-level security policy for table "catalogs" (SQLSTATE 42501)`
	traceID := trace.TraceID{0x0a, 0xf7, 0x65, 0x19, 0x16, 0xcd, 0x43, 0xdd, 0x84, 0x48, 0xeb, 0x21, 0x1c, 0x80, 0x31, 0x9c}
	tc := &crosscodexv1.TenantContext{TenantId: "tenant-1"}
	methods := []string{"ParseCatalog", "GetCatalog", "ListCatalogs", "GetControl", "SearchControls"}

	var logs *bytes.Buffer

	// rpcs calls every catalog RPC that forwards a store error.
	rpcs := func(err error) map[string]func(context.Context) error {
		stg := &storagetest.Fake{}
		Expect(stg.Put(context.Background(), "doc-1", strings.NewReader(`{"catalog":{"uuid":"test-uuid"}}`))).To(Succeed())
		logs = &bytes.Buffer{}
		svc := catalog.NewService(
			catalog.WithParser(&mockParser{items: []oscal.ControlItem{{ID: "ac-1", Title: "Policy", Text: "Develop policy", Class: oscal.ClassRequirement}}}),
			catalog.WithStore(failingStore{err: err}),
			catalog.WithStorage(stg),
			catalog.WithLogger(slog.New(slog.NewTextHandler(logs, nil))),
		)
		return map[string]func(context.Context) error{
			"ParseCatalog": func(ctx context.Context) error {
				_, e := svc.ParseCatalog(ctx, connect.NewRequest(&crosscodexv1.ParseCatalogRequest{TenantContext: tc, DocumentId: "doc-1", CatalogName: "C"}))
				return e
			},
			"GetCatalog": func(ctx context.Context) error {
				_, e := svc.GetCatalog(ctx, connect.NewRequest(&crosscodexv1.GetCatalogRequest{TenantContext: tc, CatalogId: "cat-1"}))
				return e
			},
			"ListCatalogs": func(ctx context.Context) error {
				_, e := svc.ListCatalogs(ctx, connect.NewRequest(&crosscodexv1.ListCatalogsRequest{TenantContext: tc}))
				return e
			},
			"GetControl": func(ctx context.Context) error {
				_, e := svc.GetControl(ctx, connect.NewRequest(&crosscodexv1.GetControlRequest{TenantContext: tc, ControlId: "cat-1/ac-1"}))
				return e
			},
			"SearchControls": func(ctx context.Context) error {
				_, e := svc.SearchControls(ctx, connect.NewRequest(&crosscodexv1.SearchControlsRequest{TenantContext: tc, Query: "policy"}))
				return e
			},
		}
	}

	rpcCtx := func(withTrace bool) context.Context {
		if !withTrace {
			return context.Background()
		}
		return trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: traceID, SpanID: trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8}, TraceFlags: trace.FlagsSampled,
		}))
	}

	message := func(err error) string {
		var ce *connect.Error
		Expect(errors.As(err, &ce)).To(BeTrue(), "expected a *connect.Error, got %v", err)
		return ce.Message()
	}

	It("hides a store error behind the trace ID and logs it in full, for every RPC", func() {
		calls := rpcs(errors.New(dbDetail))
		Expect(calls).To(HaveLen(len(methods)))
		for _, m := range methods {
			err := calls[m](rpcCtx(true))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal), m)
			Expect(message(err)).To(Equal(fmt.Sprintf(
				"internal catalog error; the server logged the cause under trace %s. Retry the request, and if it keeps failing, give that trace ID to an operator", traceID)), m)
			Expect(message(err)).NotTo(ContainSubstring("SQLSTATE"), m)
			Expect(logs.String()).To(ContainSubstring("method="+m), "the full error must be logged for %s", m)
		}
		Expect(logs.String()).To(ContainSubstring("level=ERROR"))
		Expect(logs.String()).To(ContainSubstring("SQLSTATE 42501"))
		Expect(logs.String()).To(ContainSubstring("trace_id=" + traceID.String()))
	})

	It("names the method when there is no trace to point at", func() {
		calls := rpcs(errors.New(dbDetail))
		for _, m := range methods {
			err := calls[m](rpcCtx(false))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal), m)
			Expect(message(err)).To(Equal(fmt.Sprintf(
				"internal catalog error; the server logged the cause. Retry the request, and if it keeps failing, give an operator the time of the failure and the method name (%s)", m)), m)
		}
	})

	It("keeps a not-found message, which the client can act on, and does not log it", func() {
		calls := rpcs(errors.New("catalog not found: cat-1"))
		err := calls["GetCatalog"](rpcCtx(true))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
		Expect(message(err)).To(Equal("catalog not found: cat-1"))
		Expect(logs.String()).To(BeEmpty())
	})
})
