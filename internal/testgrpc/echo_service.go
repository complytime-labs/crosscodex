package testgrpc

import (
	"context"
	"fmt"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/tenant"

	echopb "github.com/complytime-labs/crosscodex/internal/testgrpc/gen/echo/v1"
)

// echoServer implements the TenantEchoService.
type echoServer struct {
	echopb.UnimplementedTenantEchoServiceServer
	dbPool db.TenantConnection
	nats   natsbus.Client
}

// NewEchoService creates a TenantEchoService implementation.
// dbPool and nats may be nil if those features are not needed.
func NewEchoService(dbPool db.TenantConnection, nats natsbus.Client) echopb.TenantEchoServiceServer {
	return &echoServer{
		dbPool: dbPool,
		nats:   nats,
	}
}

func (s *echoServer) Echo(ctx context.Context, req *echopb.EchoRequest) (*echopb.EchoResponse, error) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("extract tenant: %w", err)
	}

	resp := &echopb.EchoResponse{
		TenantId: tenantID,
		UserId:   tenant.UserFromContext(ctx),
		Payload:  req.GetPayload(),
	}

	if req.GetQueryDb() && s.dbPool != nil {
		tx, err := s.dbPool.Begin(ctx)
		if err != nil {
			return nil, fmt.Errorf("db begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		rows, err := tx.Query(ctx, "SELECT job_id FROM jobs")
		if err != nil {
			return nil, fmt.Errorf("db query: %w", err)
		}
		defer rows.Close()

		var count int32
		for rows.Next() {
			var jobID string
			if err := rows.Scan(&jobID); err != nil {
				return nil, fmt.Errorf("db scan: %w", err)
			}
			count++
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("db rows: %w", err)
		}
		resp.DbRowCount = count
	}

	if req.GetPublishNats() && s.nats != nil {
		subject, err := natsbus.WorkSubject(tenantID, natsbus.TaskClassify, "echo-job")
		if err != nil {
			return nil, fmt.Errorf("nats subject: %w", err)
		}
		if err := s.nats.Publish(ctx, subject, []byte(req.GetPayload())); err != nil {
			return nil, fmt.Errorf("nats publish: %w", err)
		}
		resp.NatsSubject = subject
	}

	return resp, nil
}
