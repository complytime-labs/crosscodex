package rpcserver_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/internal/rpcserver"
)

// panicConn is a streaming handler connection; only Spec is used.
type panicConn struct{ connect.StreamingHandlerConn }

func (panicConn) Spec() connect.Spec { return connect.Spec{Procedure: "/test.v1.Test/Stream"} }

var _ = Describe("Error references", func() {
	It("generates a distinct 128-bit hex reference on every call", func() {
		a, b := rpcserver.NewErrorReference(), rpcserver.NewErrorReference()
		Expect(a).To(MatchRegexp(`^[0-9a-f]{32}$`))
		Expect(b).To(MatchRegexp(`^[0-9a-f]{32}$`))
		Expect(a).NotTo(Equal(b))
	})
})

var _ = Describe("NewRecoveryInterceptor error reference", func() {
	const panicDetail = "runtime error: secret_admin connection string in panic"
	traceID := trace.TraceID{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36}
	refPattern := regexp.MustCompile(`^internal server error; the server logged the cause under error reference ([0-9a-f]{32})\. Retry the request, and if it keeps failing, give that reference to an operator$`)

	spanCtx := func(remote bool) context.Context {
		return trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: traceID, SpanID: trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8}, TraceFlags: trace.FlagsSampled, Remote: remote,
		}))
	}

	// calls runs a panicking handler through the interceptor, unary and
	// streaming, logging to logs.
	calls := map[string]func(ctx context.Context, logs *bytes.Buffer) error{
		"unary": func(ctx context.Context, logs *bytes.Buffer) error {
			ic := rpcserver.NewRecoveryInterceptor(slog.New(slog.NewTextHandler(logs, nil)))
			_, err := ic.WrapUnary(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
				panic(panicDetail)
			})(ctx, connect.NewRequest(&struct{}{}))
			return err
		},
		"streaming": func(ctx context.Context, logs *bytes.Buffer) error {
			ic := rpcserver.NewRecoveryInterceptor(slog.New(slog.NewTextHandler(logs, nil)))
			return ic.WrapStreamingHandler(func(context.Context, connect.StreamingHandlerConn) error {
				panic(panicDetail)
			})(ctx, panicConn{})
		},
	}

	DescribeTable("returns an opaque message with a reference that the log carries, with the local trace ID",
		func(kind string) {
			logs := &bytes.Buffer{}
			err := calls[kind](spanCtx(false), logs)
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
			var ce *connect.Error
			Expect(err).To(BeAssignableToTypeOf(ce))
			ce = err.(*connect.Error)
			m := refPattern.FindStringSubmatch(ce.Message())
			Expect(m).To(HaveLen(2), "message %q must carry an error reference", ce.Message())
			Expect(ce.Message()).NotTo(ContainSubstring("secret_admin"))
			Expect(logs.String()).To(ContainSubstring("error_ref=" + m[1]))
			Expect(logs.String()).To(ContainSubstring("trace_id=" + traceID.String()))
			Expect(strings.Count(logs.String(), "secret_admin")).To(Equal(1))
		},
		Entry("unary", "unary"),
		Entry("streaming", "streaming"),
	)

	DescribeTable("does not log a remote (caller-chosen) trace ID as its own",
		func(kind string) {
			logs := &bytes.Buffer{}
			err := calls[kind](spanCtx(true), logs)
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInternal))
			Expect(logs.String()).To(ContainSubstring("error_ref="))
			Expect(logs.String()).NotTo(ContainSubstring(fmt.Sprintf("trace_id=%s", traceID)))
		},
		Entry("unary", "unary"),
		Entry("streaming", "streaming"),
	)
})
