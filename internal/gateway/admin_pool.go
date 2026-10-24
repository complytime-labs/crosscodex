package gateway

import (
	"context"
	"log/slog"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/db"
)

// PoolAdminBackend implements AdminBackend using a database connection
// pool's own health check as the sole readiness signal. Shared by every
// daemon that needs an AdminBackend backed by nothing more than "is
// Postgres reachable" (cmd/crosscodex embedded mode, cmd/crosscodexd's
// gateway role).
type PoolAdminBackend struct {
	pool db.Pool
}

// NewPoolAdminBackend returns an AdminBackend backed by pool's health check.
func NewPoolAdminBackend(pool db.Pool) *PoolAdminBackend {
	return &PoolAdminBackend{pool: pool}
}

// HealthCheck reports the database as the "database" dependency. The
// driver error is logged here and withheld from the response (CWE-209): it
// can carry the host, port and role of the connection.
func (b *PoolAdminBackend) HealthCheck(ctx context.Context, _ *connect.Request[pb.HealthCheckRequest]) (*connect.Response[pb.HealthCheckResponse], error) {
	status, message := pb.HealthStatus_HEALTH_STATUS_HEALTHY, "database reachable"
	healthStatus, err := b.pool.Health(ctx)
	if err != nil {
		slog.WarnContext(ctx, "database health check failed", "error", err)
		status, message = pb.HealthStatus_HEALTH_STATUS_UNHEALTHY, "database unreachable"
	} else if !healthStatus.Connected {
		status, message = pb.HealthStatus_HEALTH_STATUS_UNHEALTHY, "database unreachable"
	}
	return connect.NewResponse(&pb.HealthCheckResponse{
		Status: status,
		Services: map[string]*pb.ServiceHealth{
			"database": {
				ServiceName: "database",
				Status:      status,
				Message:     message,
				LastCheck:   timestamppb.Now(),
			},
		},
	}), nil
}
