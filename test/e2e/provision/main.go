// Package main runs migrations and sets the graph_user password for E2E tests.
// The password comes from TEST_GRAPH_USER_PASSWORD, which .taskfiles/test/e2e.yml
// generates and also embeds in CROSSCODEX_DATABASE_GRAPH_DSN, so nothing secret
// is printed.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run performs migration and sets the graph_user password. It returns
// errors instead of calling os.Exit so deferred Close calls always run
// (gocritic exitAfterDefer).
func run() error {
	if len(os.Args) != 2 {
		return fmt.Errorf("usage: %s <DSN>", os.Args[0])
	}

	dsn := os.Args[1]
	ctx := context.Background()

	// Run migrations
	migrator, err := db.NewMigrator(dsn)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}
	defer migrator.Close()

	if err := migrator.Up(ctx); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("run migrations: %w", err)
	}

	// Set graph_user password
	adminDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open admin connection: %w", err)
	}
	defer adminDB.Close()

	password, err := dbtest.RolePassword(dbtest.GraphUserPasswordEnv)
	if err != nil {
		return fmt.Errorf("read graph_user password: %w", err)
	}
	stmt, err := dbtest.AlterRolePasswordSQL("graph_user", password)
	if err != nil {
		return fmt.Errorf("build graph_user password statement: %w", err)
	}
	if _, err := adminDB.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("set graph_user password: %w", err)
	}
	return nil
}
