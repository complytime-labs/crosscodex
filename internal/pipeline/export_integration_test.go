//go:build integration

package pipeline

import (
	"context"

	"github.com/complytime-labs/crosscodex/pkg/db"
)

// OpenIntegrationDB bridges openIntegrationDB to the external pipeline_test
// integration specs: the superuser pool (seeding and checks that bypass
// RLS), the restricted app_user pool, and the tenant connection over it.
func OpenIntegrationDB(ctx context.Context, appConns int) (suPool, appPool db.Pool, tenantConn db.TenantConnection) {
	idb := openIntegrationDB(ctx, appConns)
	return idb.suPool, idb.appPool, idb.tenantConn
}
