package db

import (
	"context"

	"go.opentelemetry.io/otel/codes"
)

func (p *pgPool) VerifyExtensions(ctx context.Context) error {
	ctx, span := p.startSpan(ctx, "db.VerifyExtensions")
	defer span.End()

	if len(p.extensions) == 0 {
		span.SetStatus(codes.Ok, "")
		return nil
	}

	var missing []string
	for _, ext := range p.extensions {
		var name string
		err := p.db.QueryRowContext(ctx,
			"SELECT extname FROM pg_extension WHERE extname = $1", ext,
		).Scan(&name)
		if err != nil {
			missing = append(missing, ext)
		}
	}
	if len(missing) > 0 {
		span.SetStatus(codes.Error, "missing extensions")
		return &ExtensionError{Missing: missing}
	}
	span.SetStatus(codes.Ok, "")
	return nil
}
