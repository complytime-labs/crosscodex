package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"

	"github.com/complytime-labs/crosscodex/pkg/db/migrations"
)

type pgMigrator struct {
	m *migrate.Migrate
}

// NewMigrator creates a new migrator that runs embedded SQL migrations.
// It manages its own database connection, separate from the application
// pool, because golang-migrate handles connection lifecycle internally.
func NewMigrator(dsn string) (Migrator, error) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("failed to create migration source: %w", err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to create migrator: %w", err)
	}

	return &pgMigrator{m: m}, nil
}

func (mg *pgMigrator) Up(ctx context.Context) error {
	_, span := otel.GetTracerProvider().Tracer("crosscodex/pkg/db").Start(ctx, "db.MigrateUp")
	defer span.End()

	err := mg.m.Up()
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		if _, dirty, vErr := mg.m.Version(); vErr == nil && dirty {
			span.SetStatus(codes.Error, "dirty migration")
			return fmt.Errorf("%w: version is dirty, manual intervention required: %s",
				ErrMigrationDirty, err)
		}
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("migration failed: %w", err)
	}
	span.SetStatus(codes.Ok, "")
	return nil
}

func (mg *pgMigrator) Version(ctx context.Context) (uint, bool, error) {
	_, span := otel.GetTracerProvider().Tracer("crosscodex/pkg/db").Start(ctx, "db.MigrateVersion")
	defer span.End()

	version, dirty, err := mg.m.Version()
	if err != nil {
		if errors.Is(err, migrate.ErrNilVersion) {
			span.SetStatus(codes.Ok, "")
			return 0, false, nil
		}
		span.SetStatus(codes.Error, err.Error())
		return 0, false, fmt.Errorf("failed to get migration version: %w", err)
	}
	span.SetStatus(codes.Ok, "")
	return version, dirty, nil
}

func (mg *pgMigrator) Close() error {
	srcErr, dbErr := mg.m.Close()
	if srcErr != nil {
		return fmt.Errorf("failed to close migration source: %w", srcErr)
	}
	if dbErr != nil {
		return fmt.Errorf("failed to close migration database: %w", dbErr)
	}
	return nil
}
