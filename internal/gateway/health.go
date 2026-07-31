package gateway

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"go.opentelemetry.io/otel/trace"
)

func (s *Service) Health(ctx context.Context, req *connect.Request[pb.HealthRequest]) (*connect.Response[pb.HealthCheckResponse], error) {
	start := time.Now()

	if s.tracer != nil {
		var span trace.Span
		ctx, span = s.tracer.Start(ctx, "gateway.Health")
		defer span.End()
	}

	if s.admin == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("health backend not configured"))
	}

	resp, err := s.admin.HealthCheck(ctx, connect.NewRequest(&pb.HealthCheckRequest{
		Service: req.Msg.GetService(),
	}))
	if err != nil {
		return nil, err
	}

	s.recordMetrics(ctx, "Health", start, connect.Code(0))
	return resp, nil
}
