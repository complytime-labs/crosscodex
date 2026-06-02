package grpcutil_test

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/complytime-labs/crosscodex/pkg/tenant/grpcutil"
)

// stubHandler is a gRPC handler that captures the context for inspection.
func stubHandler(ctx context.Context, req any) (any, error) {
	return ctx, nil
}

func TestUnaryServerInterceptor_ValidTenant(t *testing.T) {
	interceptor := grpcutil.UnaryServerInterceptor()

	md := metadata.Pairs(grpcutil.MetadataKeyTenantID, "acme-corp")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	resp, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, stubHandler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	resultCtx := resp.(context.Context)
	id, err := tenant.FromContext(resultCtx)
	if err != nil {
		t.Fatalf("tenant not in context: %v", err)
	}
	if id != "acme-corp" {
		t.Errorf("got tenant %q, want %q", id, "acme-corp")
	}
}

func TestUnaryServerInterceptor_WithUser(t *testing.T) {
	interceptor := grpcutil.UnaryServerInterceptor()

	md := metadata.Pairs(
		grpcutil.MetadataKeyTenantID, "acme-corp",
		grpcutil.MetadataKeyUserID, "alice",
	)
	ctx := metadata.NewIncomingContext(context.Background(), md)

	resp, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, stubHandler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	resultCtx := resp.(context.Context)
	if user := tenant.UserFromContext(resultCtx); user != "alice" {
		t.Errorf("got user %q, want %q", user, "alice")
	}
}

func TestUnaryServerInterceptor_MissingTenant(t *testing.T) {
	interceptor := grpcutil.UnaryServerInterceptor()

	// No metadata at all
	_, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{}, stubHandler)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %T: %v", err, err)
	}
	if st.Code() != codes.Unauthenticated {
		t.Errorf("got code %v, want %v", st.Code(), codes.Unauthenticated)
	}
}

func TestUnaryServerInterceptor_EmptyTenantMetadata(t *testing.T) {
	interceptor := grpcutil.UnaryServerInterceptor()

	md := metadata.Pairs(grpcutil.MetadataKeyTenantID, "")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, stubHandler)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unauthenticated {
		t.Errorf("got code %v, want %v", st.Code(), codes.Unauthenticated)
	}
}

func TestUnaryServerInterceptor_MalformedTenant(t *testing.T) {
	interceptor := grpcutil.UnaryServerInterceptor()

	md := metadata.Pairs(grpcutil.MetadataKeyTenantID, "BAD!TENANT")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, stubHandler)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("got code %v, want %v", st.Code(), codes.InvalidArgument)
	}
}

func TestUnaryServerInterceptor_MetadataPresentButNoTenantKey(t *testing.T) {
	interceptor := grpcutil.UnaryServerInterceptor()

	md := metadata.Pairs("x-other-key", "value")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{}, stubHandler)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unauthenticated {
		t.Errorf("got code %v, want %v", st.Code(), codes.Unauthenticated)
	}
}

func TestUnaryClientInterceptor_InjectsTenant(t *testing.T) {
	interceptor := grpcutil.UnaryClientInterceptor()

	ctx, err := tenant.WithTenant(context.Background(), "acme-corp")
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}

	// Capture the context passed to the invoker
	var capturedCtx context.Context
	fakeInvoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		capturedCtx = ctx
		return nil
	}

	err = interceptor(ctx, "/test.Service/Method", nil, nil, nil, fakeInvoker)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	md, ok := metadata.FromOutgoingContext(capturedCtx)
	if !ok {
		t.Fatal("no outgoing metadata")
	}
	vals := md.Get(grpcutil.MetadataKeyTenantID)
	if len(vals) == 0 || vals[0] != "acme-corp" {
		t.Errorf("got tenant metadata %v, want [acme-corp]", vals)
	}
}

func TestUnaryClientInterceptor_InjectsUser(t *testing.T) {
	interceptor := grpcutil.UnaryClientInterceptor()

	ctx, err := tenant.WithTenant(context.Background(), "acme-corp")
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}
	ctx = tenant.WithUser(ctx, "bob")

	var capturedCtx context.Context
	fakeInvoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		capturedCtx = ctx
		return nil
	}

	err = interceptor(ctx, "/test.Service/Method", nil, nil, nil, fakeInvoker)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	md, _ := metadata.FromOutgoingContext(capturedCtx)
	vals := md.Get(grpcutil.MetadataKeyUserID)
	if len(vals) == 0 || vals[0] != "bob" {
		t.Errorf("got user metadata %v, want [bob]", vals)
	}
}

func TestUnaryClientInterceptor_NoTenant(t *testing.T) {
	interceptor := grpcutil.UnaryClientInterceptor()

	fakeInvoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		t.Fatal("invoker should not be called")
		return nil
	}

	err := interceptor(context.Background(), "/test.Service/Method", nil, nil, nil, fakeInvoker)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.FailedPrecondition {
		t.Errorf("got code %v, want %v", st.Code(), codes.FailedPrecondition)
	}
}

func TestUnaryClientInterceptor_PreservesExistingMetadata(t *testing.T) {
	interceptor := grpcutil.UnaryClientInterceptor()

	ctx, err := tenant.WithTenant(context.Background(), "acme-corp")
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}
	// Pre-existing outgoing metadata
	md := metadata.Pairs("x-request-id", "abc123")
	ctx = metadata.NewOutgoingContext(ctx, md)

	var capturedCtx context.Context
	fakeInvoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		capturedCtx = ctx
		return nil
	}

	err = interceptor(ctx, "/test.Service/Method", nil, nil, nil, fakeInvoker)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	outMD, _ := metadata.FromOutgoingContext(capturedCtx)
	// Tenant injected
	if vals := outMD.Get(grpcutil.MetadataKeyTenantID); len(vals) == 0 || vals[0] != "acme-corp" {
		t.Errorf("tenant not in metadata: %v", vals)
	}
	// Original metadata preserved
	if vals := outMD.Get("x-request-id"); len(vals) == 0 || vals[0] != "abc123" {
		t.Errorf("original metadata lost: %v", vals)
	}
}

func TestRoundTrip_ClientToServer(t *testing.T) {
	clientInterceptor := grpcutil.UnaryClientInterceptor()
	serverInterceptor := grpcutil.UnaryServerInterceptor()

	// Set up client context with tenant + user
	ctx, err := tenant.WithTenant(context.Background(), "acme-corp")
	if err != nil {
		t.Fatalf("WithTenant: %v", err)
	}
	ctx = tenant.WithUser(ctx, "alice")

	// Simulate client interceptor → wire → server interceptor
	var wireCtx context.Context
	fakeInvoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		// Simulate wire: convert outgoing metadata to incoming
		outMD, _ := metadata.FromOutgoingContext(ctx)
		wireCtx = metadata.NewIncomingContext(context.Background(), outMD)
		return nil
	}

	err = clientInterceptor(ctx, "/test.Service/Method", nil, nil, nil, fakeInvoker)
	if err != nil {
		t.Fatalf("client interceptor: %v", err)
	}

	// Server interceptor processes the "received" request
	resp, err := serverInterceptor(wireCtx, nil, &grpc.UnaryServerInfo{}, stubHandler)
	if err != nil {
		t.Fatalf("server interceptor: %v", err)
	}

	resultCtx := resp.(context.Context)
	id, err := tenant.FromContext(resultCtx)
	if err != nil {
		t.Fatalf("tenant not in result context: %v", err)
	}
	if id != "acme-corp" {
		t.Errorf("got tenant %q, want %q", id, "acme-corp")
	}
	if user := tenant.UserFromContext(resultCtx); user != "alice" {
		t.Errorf("got user %q, want %q", user, "alice")
	}
}
