// Package backup builds, verifies and restores manifest-based backup points
// across the three CrossCodex stores: PostgreSQL (WAL-G base backups plus
// continuous WAL archiving), tenant object storage (a content-addressed blob
// repository) and the JetStream audit streams (jsm.go snapshots).
//
// A point is complete once points/<id>/manifest.json exists; the manifest
// is written last and is the commit marker. Manifests are untrusted on read
// and decoded strictly (see Decode).
//
// Postgres is restored outside this package by deploy/db/crosscodex-db-restore
// (WAL-G needs an empty PGDATA with Postgres stopped). Restore here covers
// objects and streams. See docs/dev/backup.md.
package backup
