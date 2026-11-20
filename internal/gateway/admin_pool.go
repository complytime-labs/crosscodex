package gateway

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/db"
)

// DependencyCheck is one dependency a health probe reports under Name (for
// example "nats"). Check returns nil when the dependency is usable. Its error
// is logged and never returned to the caller: /healthz is unauthenticated and
// a dependency error can carry hosts, ports and roles (CWE-209).
type DependencyCheck struct {
	Name  string
	Check func(context.Context) error
}

// PoolAdminBackend implements AdminBackend using a database connection
// pool's own health check, plus any extra DependencyChecks, as the readiness
// signal. Shared by every daemon that needs an AdminBackend backed by
// dependency probes only (cmd/crosscodex embedded mode, cmd/crosscodexd's
// gateway and all roles).
type PoolAdminBackend struct {
	pool   db.Pool
	checks []DependencyCheck
}

// NewPoolAdminBackend returns an AdminBackend backed by pool's health check
// and checks, each reported as its own dependency.
func NewPoolAdminBackend(pool db.Pool, checks ...DependencyCheck) *PoolAdminBackend {
	return &PoolAdminBackend{pool: pool, checks: checks}
}

// HealthCheck reports the database as the "database" dependency and each
// extra check under its Name; the overall status is unhealthy if any is.
// Every check runs, so the response names every failing dependency. Errors
// are logged here and withheld from the response (CWE-209): they can carry
// the host, port and role of the connection.
func (b *PoolAdminBackend) HealthCheck(ctx context.Context, _ *connect.Request[pb.HealthCheckRequest]) (*connect.Response[pb.HealthCheckResponse], error) {
	overall := pb.HealthStatus_HEALTH_STATUS_HEALTHY
	services := make(map[string]*pb.ServiceHealth, 1+len(b.checks))
	report := func(name string, err error) {
		status, message := pb.HealthStatus_HEALTH_STATUS_HEALTHY, name+" reachable"
		if err != nil {
			slog.WarnContext(ctx, name+" health check failed", "dependency", name, "error", err)
			status, message = pb.HealthStatus_HEALTH_STATUS_UNHEALTHY, name+" unreachable"
			overall = pb.HealthStatus_HEALTH_STATUS_UNHEALTHY
		}
		services[name] = &pb.ServiceHealth{
			ServiceName: name,
			Status:      status,
			Message:     message,
			LastCheck:   timestamppb.Now(),
		}
	}

	report("database", dbHealth(ctx, b.pool))
	for _, c := range b.checks {
		report(c.Name, c.Check(ctx))
	}
	return connect.NewResponse(&pb.HealthCheckResponse{Status: overall, Services: services}), nil
}

// errDatabaseNotConnected is reported when the pool answers but says it holds
// no live connection.
var errDatabaseNotConnected = errors.New("database pool reports not connected")

// dbHealth returns nil when pool reports a live connection.
func dbHealth(ctx context.Context, pool db.Pool) error {
	status, err := pool.Health(ctx)
	if err != nil {
		return err
	}
	if !status.Connected {
		return errDatabaseNotConnected
	}
	return nil
}
