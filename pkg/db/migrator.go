package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"

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

func (mg *pgMigrator) Up(_ context.Context) error {
	err := mg.m.Up()
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		if _, dirty, vErr := mg.m.Version(); vErr == nil && dirty {
			return fmt.Errorf("%w: version is dirty, manual intervention required: %s",
				ErrMigrationDirty, err)
		}
		return fmt.Errorf("migration failed: %w", err)
	}
	return nil
}

func (mg *pgMigrator) Version(_ context.Context) (uint, bool, error) {
	version, dirty, err := mg.m.Version()
	if err != nil {
		if errors.Is(err, migrate.ErrNilVersion) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("failed to get migration version: %w", err)
	}
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
