// Package testgrpc provides a configurable gRPC test harness for
// integration testing tenant context propagation across service
// boundaries.
//
// The Harness wraps a grpc.Server with the tenant gRPC interceptor
// pre-installed, and supports optional mTLS, database (TenantPool),
// and NATS (natsbus.Client) injection via functional options.
//
// Usage:
//
//	h, err := testgrpc.NewHarness(
//	    testgrpc.WithTLS("ca.pem", "server.pem", "server-key.pem"),
//	    testgrpc.WithDB(tenantPool),
//	)
//	if err != nil { ... }
//	defer h.Stop()
//
//	if err := h.Start(); err != nil { ... }
//	conn, err := h.ClientConn()
package testgrpc
