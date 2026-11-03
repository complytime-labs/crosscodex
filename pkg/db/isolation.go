package db

import (
	"context"
	"database/sql"
)

type isolationKey struct{}

// sqlIsolation maps each IsolationLevel to its database/sql level.
var sqlIsolation = map[IsolationLevel]sql.IsolationLevel{
	IsolationReadUncommitted: sql.LevelReadUncommitted,
	IsolationReadCommitted:   sql.LevelReadCommitted,
	IsolationRepeatableRead:  sql.LevelRepeatableRead,
	IsolationSerializable:    sql.LevelSerializable,
}

// WithIsolation returns ctx carrying level for Begin: a Pool's Begin, and so
// a TenantConnection's, starts the transaction at level. It must be set at
// BEGIN: NewTenantPool scopes the tenant with a statement before the caller
// runs any, and SET TRANSACTION ISOLATION LEVEL is refused after one. An
// unknown level makes Begin fail. Scope the returned context to the one
// Begin it is meant for: every Begin given it, or a context derived from
// it, starts at level.
func WithIsolation(ctx context.Context, level IsolationLevel) context.Context {
	return context.WithValue(ctx, isolationKey{}, level)
}

// IsolationFromContext returns the level WithIsolation set on ctx, if any.
func IsolationFromContext(ctx context.Context) (IsolationLevel, bool) {
	level, ok := ctx.Value(isolationKey{}).(IsolationLevel)
	return level, ok
}
