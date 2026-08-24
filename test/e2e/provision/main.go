// Package main provides a helper to run migrations and provision graph_user for E2E tests.
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/complytime-labs/crosscodex/pkg/db"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run performs migration and graph_user provisioning, printing the graph_user
// DSN to stdout on success. It returns errors instead of calling os.Exit so
// deferred Close calls always run (gocritic exitAfterDefer).
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

	pwBytes := make([]byte, 16)
	if _, err := rand.Read(pwBytes); err != nil {
		return fmt.Errorf("generate password: %w", err)
	}
	password := hex.EncodeToString(pwBytes)

	// PostgreSQL's ALTER ROLE ... WITH PASSWORD does not accept a bound
	// parameter for the password literal (it's parsed as DDL, not a query
	// value) — the pgx driver returns a syntax error for $1 here. Direct
	// interpolation is safe: password is always a locally-generated hex
	// string (rand.Read + hex.EncodeToString above), so it cannot contain a
	// quote or any other character requiring escaping.
	stmt := fmt.Sprintf("ALTER ROLE graph_user WITH PASSWORD '%s'", password)
	if _, err := adminDB.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("set graph_user password: %w", err)
	}

	// Build and print graph_user DSN
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("parse DSN: %w", err)
	}
	u.User = url.UserPassword("graph_user", password)
	fmt.Println(u.String())
	return nil
}
