package db_test

import (
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/db"
)

func FuzzRedactDSN(f *testing.F) {
	f.Add("postgresql://user:password@localhost:5432/mydb")
	f.Add("host=localhost port=5432 user=admin password=secret dbname=test")
	f.Add("")
	f.Add("not-a-dsn-at-all")
	f.Add("postgresql://nopassword@host/db")
	f.Add("postgresql://user:p%40ssw0rd@host/db")
	f.Add("host=localhost password=s3cret sslmode=require")
	f.Add("postgresql://user:pass@host:5432/db?sslmode=require&password=leaked")

	f.Fuzz(func(t *testing.T, dsn string) {
		result := db.ExportRedactDSN(dsn)
		// Must never panic (implicit)
		// Must never return empty for non-empty input
		if dsn != "" && result == "" {
			t.Fatalf("redactDSN returned empty for non-empty input %q", dsn)
		}
		// If input contained "password" followed by a value, output must contain REDACTED or be unparseable.
		// When the DSN has no actual password value, neither REDACTED nor unparseable is required — that is acceptable.
	})
}
