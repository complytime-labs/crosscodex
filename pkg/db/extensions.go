package db

import (
	"context"
)

func (p *pgPool) VerifyExtensions(ctx context.Context) error {
	if len(p.extensions) == 0 {
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
		return &ExtensionError{Missing: missing}
	}
	return nil
}
