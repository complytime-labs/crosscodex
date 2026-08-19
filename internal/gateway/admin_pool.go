package gateway

import (
	"context"

	"connectrpc.com/connect"

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

func (b *PoolAdminBackend) HealthCheck(ctx context.Context, _ *connect.Request[pb.HealthCheckRequest]) (*connect.Response[pb.HealthCheckResponse], error) {
	healthStatus, err := b.pool.Health(ctx)
	if err != nil {
		return connect.NewResponse(&pb.HealthCheckResponse{
			Status: pb.HealthStatus_HEALTH_STATUS_UNHEALTHY,
		}), nil
	}
	if !healthStatus.Connected {
		return connect.NewResponse(&pb.HealthCheckResponse{
			Status: pb.HealthStatus_HEALTH_STATUS_UNHEALTHY,
		}), nil
	}
	return connect.NewResponse(&pb.HealthCheckResponse{
		Status: pb.HealthStatus_HEALTH_STATUS_HEALTHY,
	}), nil
}
