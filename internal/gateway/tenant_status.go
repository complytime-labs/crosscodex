package gateway

import (
	"context"

	"github.com/complytime-labs/crosscodex/pkg/db"
)

// NewPoolTenantStatus returns a TenantStatusChecker that calls
// db.TenantActive on conn, the app_user pool.
func NewPoolTenantStatus(conn db.Connection) TenantStatusChecker {
	return poolTenantStatus{conn: conn}
}

type poolTenantStatus struct{ conn db.Connection }

func (p poolTenantStatus) TenantActive(ctx context.Context, tenantID string) (bool, error) {
	return db.TenantActive(ctx, p.conn, tenantID)
}
