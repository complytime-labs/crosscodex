package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/complytime-labs/crosscodex/pkg/config"
	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// tenantSelection checks that exactly one of --tenant and --all-tenants was
// given, printing why not. The caller returns 2 on false.
func tenantSelection(cmd, tenantID string, all bool) bool {
	switch {
	case tenantID != "" && all:
		fmt.Fprintf(os.Stderr, "%s: --tenant and --all-tenants are mutually exclusive\n", cmd)
		return false
	case tenantID == "" && !all:
		fmt.Fprintf(os.Stderr, "%s: --tenant or --all-tenants is required\n", cmd)
		return false
	}
	return true
}

// tenantRunner runs a command for one tenant. ctx carries that tenant.
type tenantRunner func(ctx context.Context, id string) error

// runForTenants runs the command for tenantID or, when all is set, once per
// active tenant. It returns a process exit code.
//
// open builds the resources every tenant shares and is called once, just
// before the first tenant whose ID is valid, so a malformed tenant fails
// before any connection is made. A failing open stops the whole run: every
// tenant would fail the same way.
//
// The `tenants` RLS policy hides rows from app_user, so --all-tenants lists
// tenants through database.tenant_admin_dsn; the tenants themselves still run
// with the command's usual connections. A failing tenant does not stop the
// others: its error goes to stderr, and the run exits 1 after naming every
// failure. Each tenant's report is preceded by a "tenant: <id>" line on stdout.
func runForTenants(ctx context.Context, cmd string, cfg *config.Config, tenantID string, all bool, open func(context.Context) (tenantRunner, func(), error)) int {
	ids := []string{tenantID}
	if all {
		dsn, ok := requireTenantAdminDSN(cmd, cfg)
		if !ok {
			return 1
		}
		records, err := runTenantListFn(ctx, dsn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: list tenants: %v\n", cmd, err)
			return 1
		}
		ids = nil
		for _, r := range records {
			if r.Status == dbpkg.TenantStatusActive {
				ids = append(ids, r.ID)
			}
		}
		if len(ids) == 0 {
			fmt.Fprintf(os.Stderr, "%s: no active tenants\n", cmd)
			return 0
		}
	}

	var (
		run     tenantRunner
		closeFn func()
		failed  []string
	)
	defer func() {
		if closeFn != nil {
			closeFn()
		}
	}()
	for i, id := range ids {
		if all {
			if i > 0 {
				fmt.Fprintln(os.Stdout)
			}
			fmt.Fprintf(os.Stdout, "tenant: %s\n", displayCell(id))
		}
		tctx, err := tenant.WithTenant(ctx, id)
		if err == nil && run == nil {
			if run, closeFn, err = open(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
				return 1
			}
		}
		if err == nil {
			err = run(tctx, id)
		}
		if err == nil {
			continue
		}
		if !all {
			fmt.Fprintf(os.Stderr, "%s: %v\n", cmd, err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "%s: tenant %s: %v\n", cmd, displayCell(id), err)
		failed = append(failed, displayCell(id))
	}
	if len(failed) > 0 {
		fmt.Fprintf(os.Stderr, "%s: %d of %d tenants failed: %s\n", cmd, len(failed), len(ids), strings.Join(failed, ", "))
		return 1
	}
	return 0
}
