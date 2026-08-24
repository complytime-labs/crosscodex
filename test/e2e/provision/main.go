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
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "Usage: %s <DSN>\n", os.Args[0])
		os.Exit(1)
	}

	dsn := os.Args[1]
	ctx := context.Background()

	// Run migrations
	migrator, err := db.NewMigrator(dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create migrator: %v\n", err)
		os.Exit(1)
	}
	defer migrator.Close()

	if err := migrator.Up(ctx); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		fmt.Fprintf(os.Stderr, "Failed to run migrations: %v\n", err)
		os.Exit(1)
	}

	// Set graph_user password
	adminDB, err := sql.Open("pgx", dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to open admin connection: %v\n", err)
		os.Exit(1)
	}
	defer adminDB.Close()

	pwBytes := make([]byte, 16)
	if _, err := rand.Read(pwBytes); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to generate password: %v\n", err)
		os.Exit(1)
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
		fmt.Fprintf(os.Stderr, "Failed to set graph_user password: %v\n", err)
		os.Exit(1)
	}

	// Build and print graph_user DSN
	u, err := url.Parse(dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to parse DSN: %v\n", err)
		os.Exit(1)
	}
	u.User = url.UserPassword("graph_user", password)
	fmt.Println(u.String())
}
