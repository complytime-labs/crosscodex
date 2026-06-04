package db

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/codes"
)

func (p *pgPool) Health(ctx context.Context) (*HealthStatus, error) {
	ctx, span := p.startSpan(ctx, "db.Health")
	defer span.End()

	err := p.db.PingContext(ctx)
	stats := p.db.Stats()

	if p.connGauge != nil {
		p.connGauge.Record(ctx, int64(stats.OpenConnections))
	}

	hs := &HealthStatus{
		Connected:    err == nil,
		OpenConns:    stats.OpenConnections,
		InUse:        stats.InUse,
		Idle:         stats.Idle,
		MaxOpen:      stats.MaxOpenConnections,
		WaitCount:    stats.WaitCount,
		WaitDuration: stats.WaitDuration,
	}

	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return hs, fmt.Errorf("%w: %s", ErrPoolNotReady, err)
	}
	span.SetStatus(codes.Ok, "")
	return hs, nil
}
