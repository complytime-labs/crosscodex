package gateway

import (
	"context"
	"time"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Service) Health(ctx context.Context, req *pb.HealthRequest) (*pb.HealthCheckResponse, error) {
	start := time.Now()

	if s.tracer != nil {
		var span trace.Span
		ctx, span = s.tracer.Start(ctx, "gateway.Health")
		defer span.End()
	}

	if s.admin == nil {
		return nil, status.Error(codes.Unavailable, "health backend not configured")
	}

	resp, err := s.admin.HealthCheck(ctx, &pb.HealthCheckRequest{
		Service: req.GetService(),
	})
	if err != nil {
		return nil, err
	}

	s.recordMetrics(ctx, "Health", start, codes.OK)
	return resp, nil
}
