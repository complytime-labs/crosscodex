package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
)

// BackupRetentionLockName names the session advisory lock that keeps
// `crosscodexd admin backup run` and a non-dry-run retention scan apart. A
// purge during a backup could delete an object after Postgres was captured
// but before the objects step copied it, leaving a restored row that
// references a missing object.
const BackupRetentionLockName = "crosscodex:backup-retention"

// ErrLockHeld means another session holds the advisory lock.
var ErrLockHeld = errors.New("backup/retention in progress, retry later")

// AdvisoryLockKey returns the 64-bit FNV-1a hash of name, the key passed to
// pg_try_advisory_lock.
func AdvisoryLockKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name)) // hash.Hash.Write never returns an error
	return int64(h.Sum64())      //nolint:gosec // G115: the bit pattern is the key; wraparound is intended
}

// AdvisoryLocker takes a session-level advisory lock on its own dedicated
// connection, so the lock survives transaction boundaries and lasts until
// release (or until the connection drops). Advisory locks need no GRANT,
// so restricted roles (app_user, backup_user) can use them. Locks are
// per-database: every holder must connect to the same database.
type AdvisoryLocker struct {
	dsn  string
	name string
}

// NewAdvisoryLocker returns a locker for name on the database at dsn.
func NewAdvisoryLocker(dsn, name string) *AdvisoryLocker {
	return &AdvisoryLocker{dsn: dsn, name: name}
}

// TryAcquire takes the lock without waiting. When another session holds it,
// the error wraps ErrLockHeld. The returned release unlocks and closes the
// dedicated connection; call it exactly once.
func (l *AdvisoryLocker) TryAcquire(ctx context.Context) (func(context.Context) error, error) {
	sqlDB, err := sql.Open("pgx", l.dsn)
	if err != nil {
		return nil, fmt.Errorf("advisory lock %q: open database (%s): %w", l.name, redactDSN(l.dsn), err)
	}
	sqlDB.SetMaxOpenConns(1)
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("advisory lock %q: connect (%s): %w", l.name, redactDSN(l.dsn), err)
	}
	key := AdvisoryLockKey(l.name)
	var acquired bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		_ = conn.Close()
		_ = sqlDB.Close()
		return nil, fmt.Errorf("advisory lock %q: %w", l.name, err)
	}
	if !acquired {
		_ = conn.Close()
		_ = sqlDB.Close()
		return nil, fmt.Errorf("advisory lock %q: %w", l.name, ErrLockHeld)
	}
	release := func(ctx context.Context) error {
		var unlocked bool
		uerr := conn.QueryRowContext(ctx, "SELECT pg_advisory_unlock($1)", key).Scan(&unlocked)
		// Closing the connection ends the session, which frees the lock even if the unlock query failed.
		cerr := errors.Join(conn.Close(), sqlDB.Close())
		if uerr != nil {
			return errors.Join(fmt.Errorf("advisory lock %q: unlock: %w", l.name, uerr), cerr)
		}
		if !unlocked {
			return errors.Join(fmt.Errorf("advisory lock %q: unlock: this session did not hold the lock", l.name), cerr)
		}
		return cerr
	}
	return release, nil
}
