package db

import (
	"context"
	"fmt"
)

func (p *pgPool) Health(ctx context.Context) (*HealthStatus, error) {
	err := p.db.PingContext(ctx)
	stats := p.db.Stats()

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
		return hs, fmt.Errorf("%w: %s", ErrPoolNotReady, err)
	}
	return hs, nil
}
