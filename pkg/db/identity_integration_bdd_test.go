//go:build integration

package db_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// Judgements are permanent (#195): artifact_pair_verdicts
// rows and the tenants they belong to are never hard-deleted. app_user is
// refused by privileges; the superuser table owner is refused by triggers. The suite entry
// point lives in db_integration_bdd_test.go.
var _ = Describe("Judgements are never deleted", Ordered, func() {
	const tenantID = "idl-a"
	ctx := context.Background()
	var appConn *sql.DB

	// verdictCount and tenantStatus read as the superuser, so RLS hides nothing.
	verdictCount := func(id string) int {
		var n int
		Expect(suPool.QueryRow(ctx, "SELECT count(*) FROM artifact_pair_verdicts WHERE tenant_id = $1", id).Scan(&n)).To(Succeed())
		return n
	}
	tenantStatus := func(id string) string {
		var status string
		Expect(suPool.QueryRow(ctx, "SELECT status FROM tenants WHERE tenant_id = $1", id).Scan(&status)).To(Succeed())
		return status
	}
	// execAsAppUserInTenant runs stmt as app_user inside tenantID's RLS scope
	// and always rolls back, so a statement that unexpectedly succeeds still
	// leaves the data as it was for the next entry.
	execAsAppUserInTenant := func(id, stmt string) error {
		tx, err := appConn.BeginTx(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		defer tx.Rollback() //nolint:errcheck // probe transaction, always rolled back
		_, err = tx.ExecContext(ctx, "SELECT set_config('app.current_tenant', $1, true)", id)
		Expect(err).NotTo(HaveOccurred())
		_, err = tx.ExecContext(ctx, stmt)
		return err
	}

	BeforeAll(func() {
		if suPool == nil {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}
		appConn = appUserConn()
		setupTenant(tenantID, "Identity Lifecycle A")
		Expect(suPool.Exec(ctx, `INSERT INTO artifact_pair_verdicts
			(tenant_id, pair_key, low_group_id, high_group_id, candidate_edge_id, similarity_score, context,
			 determined_by, next_attempt_at, created_at)
			VALUES ($1, 'k', 'a', 'b', 'e', 0.5, '{}', 'x', now(), now())`, tenantID)).To(Succeed())
	})

	AfterAll(func() {
		if suPool != nil {
			stmt, err := dbtest.HardDeleteTenantSQL(tenantID)
			Expect(err).NotTo(HaveOccurred())
			Expect(suPool.Exec(ctx, stmt)).To(Succeed())
		}
	})

	DescribeTable("app_user is refused",
		func(stmt, wantCode, wantMsg string) {
			err := execAsAppUserInTenant(tenantID, stmt)
			Expect(err).To(HaveOccurred())
			Expect(db.IsPgErrorCode(err, wantCode)).To(BeTrue(), "got %v", err)
			Expect(err.Error()).To(ContainSubstring(wantMsg))
			Expect(verdictCount(tenantID)).To(Equal(1))
			Expect(tenantStatus(tenantID)).To(Equal("active"))
		},
		Entry("DELETE FROM tenants", "DELETE FROM tenants", "42501", "permission denied"),
		Entry("UPDATE tenants status", "UPDATE tenants SET status = 'active'", "42501", "permission denied"),
		Entry("DELETE verdicts", "DELETE FROM artifact_pair_verdicts", "42501", "permission denied"),
		Entry("TRUNCATE verdicts", "TRUNCATE artifact_pair_verdicts", "42501", "permission denied"),
		Entry("disable verdict trigger", "ALTER TABLE artifact_pair_verdicts DISABLE TRIGGER artifact_pair_verdicts_refuse_delete", "42501", "must be owner"),
		Entry("add cascade FK", "ALTER TABLE artifact_pair_verdicts ADD CONSTRAINT x FOREIGN KEY (tenant_id) REFERENCES tenants(tenant_id) ON DELETE CASCADE", "42501", "must be owner"),
	)

	// suPool is the superuser the migrations run as, so it also owns the
	// tables: no privilege check stops it, only the refusal triggers.
	DescribeTable("the superuser table owner is refused by triggers",
		func(stmt, wantMsg string) {
			// A short lock_timeout keeps a statement waiting on another suite's
			// lock (TRUNCATE takes ACCESS EXCLUSIVE) from stalling this one.
			tx, err := suPool.Begin(ctx)
			Expect(err).NotTo(HaveOccurred())
			defer tx.Rollback() //nolint:errcheck // probe transaction, always rolled back
			Expect(tx.Exec(ctx, "SET LOCAL lock_timeout = '5s'")).To(Succeed())
			err = tx.Exec(ctx, stmt)
			Expect(err).To(HaveOccurred())
			Expect(db.IsPgErrorCode(err, "42501")).To(BeTrue(), "got %v", err)
			Expect(err.Error()).To(ContainSubstring(wantMsg))
			Expect(verdictCount(tenantID)).To(Equal(1))
			Expect(tenantStatus(tenantID)).To(Equal("active"))
		},
		Entry("DELETE verdicts", "DELETE FROM artifact_pair_verdicts WHERE tenant_id = '"+tenantID+"'", "crosscodexd admin tenant delete"),
		Entry("TRUNCATE verdicts", "TRUNCATE artifact_pair_verdicts", "never deleted"),
		Entry("DELETE tenant", "DELETE FROM tenants WHERE tenant_id = '"+tenantID+"'", "soft-delete"),
		Entry("TRUNCATE tenants", "TRUNCATE tenants CASCADE", "never hard-deleted"),
	)

	It("declares the verdict FK ON DELETE RESTRICT explicitly", func() {
		var action string
		Expect(suPool.QueryRow(ctx, `SELECT confdeltype FROM pg_constraint
			WHERE conrelid = 'artifact_pair_verdicts'::regclass AND contype = 'f'
			  AND confrelid = 'tenants'::regclass`).Scan(&action)).To(Succeed())
		Expect(action).To(Equal("r")) // r = RESTRICT, a = NO ACTION
	})
})

// signed returns canonical request bytes, built by db.IdentityRequest so
// the database's request binding is tested against the Go encoding, and a
// link envelope whose single material digest is sha256(request).
func signed(gen attestation.Generator, action, kind, tenantID, identity, actor, reason string) (req, env []byte) {
	req, err := db.IdentityRequest{
		Action: action, Kind: kind, TenantID: tenantID, IdentityID: identity,
		Actor: actor, Reason: reason, RequestedAt: time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC),
	}.Canonical()
	Expect(err).NotTo(HaveOccurred())
	sum := sha256.Sum256(req)
	link, err := gen.CreateLink(context.Background(), "identity."+action,
		[]attestation.Artifact{{URI: "crosscodex:identity-request", Digest: hex.EncodeToString(sum[:])}}, nil)
	Expect(err).NotTo(HaveOccurred())
	return req, link.Raw
}

// signedRequest is signed as a db.SignedRequest.
func signedRequest(gen attestation.Generator, action, kind, tenantID, identity, actor, reason string) db.SignedRequest {
	req, env := signed(gen, action, kind, tenantID, identity, actor, reason)
	return db.SignedRequest{Request: req, Attestation: env}
}

// eventCount counts tenant id's identity_events rows, read as the superuser.
func eventCount(id string) int {
	var n int
	Expect(suPool.QueryRow(context.Background(),
		"SELECT count(*) FROM identity_events WHERE tenant_id = $1", id).Scan(&n)).To(Succeed())
	return n
}

// tenantSnapshot is tenant id's whole tenants row as JSON ("" when
// absent), read as the superuser so RLS hides nothing.
func tenantSnapshot(id string) string {
	var row string
	Expect(suPool.QueryRow(context.Background(),
		"SELECT coalesce((SELECT row_to_json(t)::text FROM tenants t WHERE tenant_id = $1), '')", id).Scan(&row)).To(Succeed())
	return row
}

// Tenant lifecycle (#195): tenant_admin soft-deletes, restores,
// hides and unhides tenants only through SECURITY DEFINER functions, each of
// which appends a signed event to the append-only identity_events table, and
// enter_tenant refuses a deleted tenant on every app path. These specs call
// the SQL functions directly; the Go wrappers are tested with pkg/db's API.
var _ = Describe("Tenant lifecycle", Ordered, func() {
	const (
		liveID    = "idl-live"
		deletedID = "idl-del"
		unknownID = "idl-nobody"
		actor     = "ops@example.invalid"
		reason    = "customer offboarded"
	)
	ctx := context.Background()
	var (
		gen       attestation.Generator
		adminConn *sql.DB
		appConn   *sql.DB
		graphConn *sql.DB
	)
	connFor := func(role string) *sql.DB {
		return map[string]*sql.DB{"app_user": appConn, "graph_user": graphConn, "tenant_admin": adminConn}[role]
	}

	eventActions := func(id string) []string {
		rows, err := suPool.Query(ctx, "SELECT action FROM identity_events WHERE tenant_id = $1 ORDER BY event_id", id)
		Expect(err).NotTo(HaveOccurred())
		defer rows.Close()
		var actions []string
		for rows.Next() {
			var a string
			Expect(rows.Scan(&a)).To(Succeed())
			actions = append(actions, a)
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		return actions
	}
	tenantField := func(id, column string) sql.NullString {
		var v sql.NullString
		Expect(suPool.QueryRow(ctx,
			"SELECT (row_to_json(t) ->> $2::text) FROM tenants t WHERE tenant_id = $1", id, column).Scan(&v)).To(Succeed())
		return v
	}

	// callTenantFn signs a request for action and calls <action>_tenant as conn.
	callTenantFn := func(conn *sql.DB, action, id string) error {
		req, env := signed(gen, action, "tenant", id, id, actor, reason)
		_, err := conn.ExecContext(ctx, "SELECT public."+action+"_tenant($1, $2, $3, $4, $5)", id, actor, reason, req, env)
		return err
	}
	hideTenant := func(id string) (string, error) {
		req, env := signed(gen, "hide", "tenant", id, id, actor, reason)
		var pseudonym string
		err := adminConn.QueryRowContext(ctx, "SELECT public.hide_tenant($1, $2, $3, $4, $5)", id, actor, reason, req, env).Scan(&pseudonym)
		return pseudonym, err
	}
	// expectRefusedUnchanged runs call and asserts the SQLSTATE, a message
	// substring, and that id's tenants row and event count did not change.
	expectRefusedUnchanged := func(id string, call func() error, code, msg string) {
		before, events := tenantSnapshot(id), eventCount(id)
		err := call()
		Expect(err).To(HaveOccurred())
		Expect(db.IsPgErrorCode(err, code)).To(BeTrue(), "want SQLSTATE %s, got %v", code, err)
		Expect(err.Error()).To(ContainSubstring(msg))
		Expect(tenantSnapshot(id)).To(Equal(before))
		Expect(eventCount(id)).To(Equal(events))
	}
	// currentTenantAfterEnter enters id as conn and reads app.current_tenant
	// in the same transaction, which it always rolls back.
	currentTenantAfterEnter := func(conn *sql.DB, id string) (string, error) {
		tx, err := conn.BeginTx(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		defer tx.Rollback() //nolint:errcheck // probe transaction, always rolled back
		if _, err := tx.ExecContext(ctx, "SELECT public.enter_tenant($1)", id); err != nil {
			return "", err
		}
		var current string
		Expect(tx.QueryRowContext(ctx, "SELECT current_setting('app.current_tenant', true)").Scan(&current)).To(Succeed())
		return current, nil
	}

	BeforeAll(func() {
		if suPool == nil {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}
		kp, err := attestation.NewEphemeralKeyProvider()
		Expect(err).NotTo(HaveOccurred())
		gen, err = attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())
		adminConn, appConn, graphConn = tenantAdminConn(), appUserConn(), graphUserConn()
		setupTenant(liveID, "Lifecycle Live")
		setupTenant(deletedID, "Lifecycle Deleted")
	})

	AfterAll(func() {
		if suPool != nil {
			for _, id := range []string{liveID, deletedID} {
				stmt, err := dbtest.HardDeleteTenantSQL(id)
				Expect(err).NotTo(HaveOccurred())
				Expect(suPool.Exec(ctx, stmt)).To(Succeed())
			}
		}
	})

	DescribeTable("new functions are not executable by PUBLIC",
		func(signature string) {
			var ok bool
			Expect(suPool.QueryRow(ctx, "SELECT has_function_privilege('public', $1, 'EXECUTE')", signature).Scan(&ok)).To(Succeed())
			Expect(ok).To(BeFalse(), "PUBLIC must not hold EXECUTE on %s", signature)
		},
		Entry(nil, "public.record_identity_event(text, text, text, text, text, text, text, bytea, bytea)"),
		Entry(nil, "public.delete_tenant(text, text, text, bytea, bytea)"),
		Entry(nil, "public.restore_tenant(text, text, text, bytea, bytea)"),
		Entry(nil, "public.hide_tenant(text, text, text, bytea, bytea)"),
		Entry(nil, "public.unhide_tenant(text, text, text, bytea, bytea)"),
		Entry(nil, "public.list_tenants()"),
		Entry(nil, "public.enter_tenant(text)"),
		Entry(nil, "public.list_retention_tenants()"),
	)

	It("declares every definer function with a pinned search_path", func() {
		rows, err := suPool.Query(ctx, `
			SELECT proname, prosecdef, coalesce(array_to_string(proconfig, ';'), '')
			FROM pg_proc
			WHERE pronamespace = 'public'::regnamespace
			  AND proname IN ('delete_tenant', 'restore_tenant', 'hide_tenant', 'unhide_tenant',
			                  'list_tenants', 'set_tenant_status', 'provision_tenant', 'enter_tenant',
			                  'list_retention_tenants')
			ORDER BY proname`)
		Expect(err).NotTo(HaveOccurred())
		defer rows.Close()
		var names []string
		for rows.Next() {
			var name, config string
			var definer bool
			Expect(rows.Scan(&name, &definer, &config)).To(Succeed())
			Expect(definer).To(BeTrue(), name)
			Expect(config).To(Equal("search_path=pg_catalog, public, pg_temp"), name)
			names = append(names, name)
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		Expect(names).To(HaveLen(9))
	})

	DescribeTable("reserves the 'hidden-' tenant ID prefix for pseudonyms",
		func(id string, call func(id string) error, wantCode, wantMsg string) {
			expectRefusedUnchanged(id, func() error { return call(id) }, wantCode, wantMsg)
			Expect(tenantSnapshot(id)).To(BeEmpty())
		},
		Entry("provision_tenant with a pseudonym-shaped ID", "hidden-tenant-0123456789abcdef", func(id string) error {
			_, err := adminConn.ExecContext(ctx, "SELECT public.provision_tenant($1, 'Impostor')", id)
			return err
		}, "22023", "reserved for pseudonyms of hidden tenants; choose another ID"),
		Entry("provision_tenant with another hidden- ID", "hidden-x1", func(id string) error {
			_, err := adminConn.ExecContext(ctx, "SELECT public.provision_tenant($1, 'Impostor')", id)
			return err
		}, "22023", "reserved for pseudonyms"),
		Entry("a direct insert, as EnsureTenant does", "hidden-x2", func(id string) error {
			return suPool.Exec(ctx, "INSERT INTO tenants (tenant_id, display_name) VALUES ($1, 'Impostor')", id)
		}, "23514", "tenants_id_not_reserved"),
	)

	DescribeTable("delete_tenant refuses invalid operator input and changes nothing",
		func(build func() (callActor, callReason string, req, env []byte), wantMsg string) {
			callActor, callReason, req, env := build()
			expectRefusedUnchanged(liveID, func() error {
				_, err := adminConn.ExecContext(ctx, "SELECT public.delete_tenant($1, $2, $3, $4, $5)",
					liveID, callActor, callReason, req, env)
				return err
			}, "22023", wantMsg)
			Expect(tenantField(liveID, "status").String).To(Equal("active"))
		},
		Entry("request action differs from the call", func() (string, string, []byte, []byte) {
			req, env := signed(gen, "restore", "tenant", liveID, liveID, actor, reason)
			return actor, reason, req, env
		}, "does not match"),
		Entry("request actor differs from the call", func() (string, string, []byte, []byte) {
			req, env := signed(gen, "delete", "tenant", liveID, liveID, "mallory", reason)
			return actor, reason, req, env
		}, "does not match"),
		Entry("request reason differs from the call", func() (string, string, []byte, []byte) {
			req, env := signed(gen, "delete", "tenant", liveID, liveID, actor, "other reason")
			return actor, reason, req, env
		}, "does not match"),
		Entry("request signed for another tenant, replayed", func() (string, string, []byte, []byte) {
			req, env := signed(gen, "delete", "tenant", deletedID, deletedID, actor, reason)
			return actor, reason, req, env
		}, "does not match the change: its identity_id, tenant_id differ"),
		Entry("request kind differs from the call", func() (string, string, []byte, []byte) {
			req, env := signed(gen, "delete", "reviewer", liveID, liveID, actor, reason)
			return actor, reason, req, env
		}, "does not match the change: its kind differs"),
		Entry("request identity differs from the call", func() (string, string, []byte, []byte) {
			req, env := signed(gen, "delete", "tenant", liveID, "someone-else", actor, reason)
			return actor, reason, req, env
		}, "does not match the change: its identity_id differs"),
		Entry("request larger than 4096 bytes", func() (string, string, []byte, []byte) {
			req, env := signed(gen, "delete", "tenant", liveID, liveID, actor, reason)
			// Trailing whitespace keeps the request valid JSON, so only its size is wrong.
			return actor, reason, append(req, []byte(strings.Repeat(" ", 4097-len(req)))...), env
		}, "request is larger than 4096 bytes"),
		Entry("attestation larger than 65536 bytes", func() (string, string, []byte, []byte) {
			req, env := signed(gen, "delete", "tenant", liveID, liveID, actor, reason)
			return actor, reason, req, append(env, []byte(strings.Repeat(" ", 65537-len(env)))...)
		}, "attestation is larger than 65536 bytes"),
		Entry("blank reason", func() (string, string, []byte, []byte) {
			req, env := signed(gen, "delete", "tenant", liveID, liveID, actor, "   ")
			return actor, "   ", req, env
		}, "reason (--reason)"),
		Entry("reason with a control character", func() (string, string, []byte, []byte) {
			req, env := signed(gen, "delete", "tenant", liveID, liveID, actor, "bad\x01reason")
			return actor, "bad\x01reason", req, env
		}, "reason (--reason)"),
		Entry("501-byte reason", func() (string, string, []byte, []byte) {
			long := strings.Repeat("r", 501)
			req, env := signed(gen, "delete", "tenant", liveID, liveID, actor, long)
			return actor, long, req, env
		}, "reason (--reason)"),
		Entry("blank actor", func() (string, string, []byte, []byte) {
			req, env := signed(gen, "delete", "tenant", liveID, liveID, "", reason)
			return "", reason, req, env
		}, "actor (--by)"),
		Entry("actor with a leading space", func() (string, string, []byte, []byte) {
			req, env := signed(gen, "delete", "tenant", liveID, liveID, " ops", reason)
			return " ops", reason, req, env
		}, "actor (--by)"),
		Entry("empty attestation", func() (string, string, []byte, []byte) {
			req, _ := signed(gen, "delete", "tenant", liveID, liveID, actor, reason)
			return actor, reason, req, []byte{}
		}, "no attestation"),
		Entry("request that is not JSON", func() (string, string, []byte, []byte) {
			_, env := signed(gen, "delete", "tenant", liveID, liveID, actor, reason)
			return actor, reason, []byte("not json"), env
		}, "not valid UTF-8 JSON"),
	)

	DescribeTable("refuses identity actions on an unknown tenant",
		func(action string) {
			expectRefusedUnchanged(unknownID, func() error { return callTenantFn(adminConn, action, unknownID) },
				"P0002", "does not exist")
			Expect(tenantSnapshot(unknownID)).To(BeEmpty())
		},
		Entry(nil, "delete"),
		Entry(nil, "restore"),
		Entry(nil, "hide"),
		Entry(nil, "unhide"),
	)

	DescribeTable("refuses identity actions by a non-admin role",
		func(role, action string) {
			expectRefusedUnchanged(liveID, func() error { return callTenantFn(connFor(role), action, liveID) },
				"42501", "permission denied")
		},
		Entry(nil, "app_user", "delete"),
		Entry(nil, "app_user", "restore"),
		Entry(nil, "app_user", "hide"),
		Entry(nil, "app_user", "unhide"),
		Entry(nil, "graph_user", "delete"),
		Entry(nil, "graph_user", "restore"),
		Entry(nil, "graph_user", "hide"),
		Entry(nil, "graph_user", "unhide"),
	)

	DescribeTable("refuses identity actions that do not apply to an active tenant",
		func(action, wantMsg string) {
			expectRefusedUnchanged(liveID, func() error { return callTenantFn(adminConn, action, liveID) },
				"55000", wantMsg)
		},
		Entry(nil, "restore", "is not deleted"),
		Entry(nil, "hide", "only a deleted tenant"),
		Entry(nil, "unhide", "is not hidden"),
	)

	DescribeTable("enter_tenant scopes the same transaction to a tenant that is not deleted",
		func(role, id string) {
			current, err := currentTenantAfterEnter(connFor(role), id)
			Expect(err).NotTo(HaveOccurred())
			Expect(current).To(Equal(id))
		},
		Entry("app_user, active tenant", "app_user", liveID),
		Entry("app_user, unknown tenant", "app_user", unknownID),
		Entry("graph_user, active tenant", "graph_user", liveID),
	)

	It("soft-deletes a tenant and records a verifiable signed event", func() {
		long := strings.Repeat("r", 500) // the longest reason allowed
		req, env := signed(gen, "delete", "tenant", deletedID, deletedID, actor, long)
		_, err := adminConn.ExecContext(ctx, "SELECT public.delete_tenant($1, $2, $3, $4, $5)", deletedID, actor, long, req, env)
		Expect(err).NotTo(HaveOccurred())

		Expect(tenantField(deletedID, "status").String).To(Equal("deleted"))
		Expect(tenantField(deletedID, "deleted_by").String).To(Equal(actor))
		Expect(tenantField(deletedID, "deleted_at").Valid).To(BeTrue())

		var action, kind, identity, gotActor, dbUser, gotReason, attSHA string
		var gotReq, gotEnv []byte
		Expect(suPool.QueryRow(ctx, `SELECT action, kind, identity_id, actor, db_user, reason, request, attestation, attestation_sha256
			FROM identity_events WHERE tenant_id = $1`, deletedID).
			Scan(&action, &kind, &identity, &gotActor, &dbUser, &gotReason, &gotReq, &gotEnv, &attSHA)).To(Succeed())
		Expect([]string{action, kind, identity, gotActor, dbUser, gotReason}).
			To(Equal([]string{"delete", "tenant", deletedID, actor, "tenant_admin", long}))
		Expect(gotReq).To(Equal(req))
		envSum := sha256.Sum256(gotEnv)
		Expect(attSHA).To(Equal(hex.EncodeToString(envSum[:])))

		verified, err := gen.Verify(ctx, gotEnv)
		Expect(err).NotTo(HaveOccurred())
		reqSum := sha256.Sum256(gotReq)
		Expect(verified.Materials).To(HaveLen(1))
		Expect(verified.Materials[0].Digest).To(Equal(hex.EncodeToString(reqSum[:])))
	})

	It("refuses to delete a tenant twice", func() {
		expectRefusedUnchanged(deletedID, func() error { return callTenantFn(adminConn, "delete", deletedID) },
			"55000", "already deleted")
		Expect(eventCount(deletedID)).To(Equal(1))
	})

	DescribeTable("enter_tenant refuses a deleted tenant",
		func(role string) {
			expectRefusedUnchanged(deletedID, func() error {
				_, err := currentTenantAfterEnter(connFor(role), deletedID)
				return err
			}, "42501", "admin tenant restore")
		},
		Entry(nil, "app_user"),
		Entry(nil, "graph_user"),
	)

	It("refuses TenantPool.Begin for a deleted tenant", func() {
		appPool, err := db.NewPool(db.PoolConfig{DSN: appUserDSN()})
		Expect(err).NotTo(HaveOccurred())
		defer appPool.Close()
		scoped, err := tenant.WithTenant(ctx, deletedID)
		Expect(err).NotTo(HaveOccurred())
		before := tenantSnapshot(deletedID)

		tx, err := db.NewTenantPool(appPool).Begin(scoped)
		if tx != nil {
			_ = tx.Rollback()
		}
		Expect(tx).To(BeNil())
		Expect(err).To(HaveOccurred())
		Expect(db.IsPgErrorCode(err, "42501")).To(BeTrue(), "want insufficient_privilege, got %v", err)
		Expect(err.Error()).To(ContainSubstring("admin tenant restore"))
		Expect(tenantSnapshot(deletedID)).To(Equal(before), "the refused Begin must not change the tenant row")
	})

	It("lets NewRetentionTenantPool begin for a deleted tenant while NewTenantPool still refuses", func() {
		appPool, err := db.NewPool(db.PoolConfig{DSN: appUserDSN()})
		Expect(err).NotTo(HaveOccurred())
		defer appPool.Close()
		scoped, err := tenant.WithTenant(ctx, deletedID)
		Expect(err).NotTo(HaveOccurred())

		tx, err := db.NewRetentionTenantPool(appPool).Begin(scoped)
		Expect(err).NotTo(HaveOccurred())
		var current string
		Expect(tx.QueryRow(scoped, "SELECT current_setting('app.current_tenant')").Scan(&current)).To(Succeed())
		Expect(current).To(Equal(deletedID))
		Expect(tx.Rollback()).To(Succeed())

		denied, err := db.NewTenantPool(appPool).Begin(scoped)
		if denied != nil {
			_ = denied.Rollback()
		}
		Expect(db.IsPgErrorCode(err, "42501")).To(BeTrue(), "want insufficient_privilege, got %v", err)
	})

	It("refuses set_tenant_status on a deleted tenant", func() {
		expectRefusedUnchanged(deletedID, func() error {
			_, err := adminConn.ExecContext(ctx, "SELECT public.set_tenant_status($1, 'active')", deletedID)
			return err
		}, "55000", "restore")
		Expect(tenantField(deletedID, "status").String).To(Equal("deleted"))
	})

	It("refuses to rename a deleted tenant through provision_tenant", func() {
		expectRefusedUnchanged(deletedID, func() error {
			_, err := adminConn.ExecContext(ctx, "SELECT public.provision_tenant($1, 'Renamed')", deletedID)
			return err
		}, "55000", "restore")
		Expect(tenantField(deletedID, "display_name").String).To(Equal("Lifecycle Deleted"))
	})

	var pseudonym string

	It("hides a deleted tenant behind a pseudonym in list_tenants", func() {
		var err error
		pseudonym, err = hideTenant(deletedID)
		Expect(err).NotTo(HaveOccurred())
		Expect(pseudonym).To(MatchRegexp(`^hidden-tenant-[0-9a-f]{16}$`))

		var name string
		var hidden, hasDeletedAt bool
		Expect(adminConn.QueryRowContext(ctx,
			"SELECT display_name, hidden, deleted_at IS NOT NULL FROM list_tenants() WHERE tenant_id = $1", pseudonym).
			Scan(&name, &hidden, &hasDeletedAt)).To(Succeed())
		Expect(name).To(BeEmpty())
		Expect(hidden).To(BeTrue())
		Expect(hasDeletedAt).To(BeTrue())

		var realIDListed bool
		Expect(adminConn.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT 1 FROM list_tenants() WHERE tenant_id = $1)", deletedID).Scan(&realIDListed)).To(Succeed())
		Expect(realIDListed).To(BeFalse())
		Expect(eventActions(deletedID)).To(Equal([]string{"delete", "hide"}))
	})

	It("list_retention_tenants shows every tenant, a hidden one by pseudonym, to tenant_admin only", func() {
		rows, err := adminConn.QueryContext(ctx, "SELECT tenant_id, display_id, status FROM list_retention_tenants()")
		Expect(err).NotTo(HaveOccurred())
		defer rows.Close()
		listed := map[string][2]string{}
		for rows.Next() {
			var id, display, status string
			Expect(rows.Scan(&id, &display, &status)).To(Succeed())
			listed[id] = [2]string{display, status}
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		Expect(listed).To(HaveKeyWithValue(liveID, [2]string{liveID, "active"}))
		Expect(listed).To(HaveKeyWithValue(deletedID, [2]string{pseudonym, "deleted"}))

		for _, role := range []string{"app_user", "graph_user"} {
			_, err := connFor(role).ExecContext(ctx, "SELECT * FROM list_retention_tenants()")
			Expect(db.IsPgErrorCode(err, "42501")).To(BeTrue(), "%s: want insufficient_privilege, got %v", role, err)
		}
	})

	DescribeTable("refuses identity actions that do not apply to a hidden tenant",
		func(action, wantMsg string) {
			expectRefusedUnchanged(deletedID, func() error { return callTenantFn(adminConn, action, deletedID) },
				"55000", wantMsg)
		},
		Entry(nil, "hide", "already hidden"),
		Entry(nil, "restore", "unhide"),
		Entry(nil, "delete", "already deleted"),
	)

	It("unhides, and hiding again reuses the same pseudonym", func() {
		Expect(callTenantFn(adminConn, "unhide", deletedID)).To(Succeed())
		Expect(tenantField(deletedID, "hidden_at").Valid).To(BeFalse())
		var listed bool
		Expect(adminConn.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT 1 FROM list_tenants() WHERE tenant_id = $1 AND NOT hidden)", deletedID).Scan(&listed)).To(Succeed())
		Expect(listed).To(BeTrue())

		again, err := hideTenant(deletedID)
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(Equal(pseudonym))
		Expect(callTenantFn(adminConn, "unhide", deletedID)).To(Succeed())
		Expect(eventActions(deletedID)).To(Equal([]string{"delete", "hide", "unhide", "hide", "unhide"}))
	})

	It("restores a deleted tenant as suspended and records the event", func() {
		Expect(callTenantFn(adminConn, "restore", deletedID)).To(Succeed())
		Expect(tenantField(deletedID, "status").String).To(Equal("suspended"))
		Expect(tenantField(deletedID, "deleted_at").Valid).To(BeFalse())
		Expect(tenantField(deletedID, "deleted_by").Valid).To(BeFalse())
		Expect(tenantField(deletedID, "pseudonym").String).To(Equal(pseudonym))
		Expect(eventActions(deletedID)).To(Equal([]string{"delete", "hide", "unhide", "hide", "unhide", "restore"}))

		current, err := currentTenantAfterEnter(appConn, deletedID)
		Expect(err).NotTo(HaveOccurred())
		Expect(current).To(Equal(deletedID))
	})

	DescribeTable("no application role can read, forge or change identity_events",
		func(role, stmt string) {
			expectRefusedUnchanged(deletedID, func() error {
				_, err := connFor(role).ExecContext(ctx, stmt)
				return err
			}, "42501", "permission denied")
		},
		Entry(nil, "app_user", "SELECT count(*) FROM identity_events"),
		Entry(nil, "app_user", "INSERT INTO identity_events (action, kind, tenant_id, identity_id, actor, reason, request, attestation) VALUES ('delete', 'tenant', 'idl-del', 'idl-del', 'x', 'x', '\\x7b7d', '\\x00')"),
		Entry(nil, "app_user", "UPDATE identity_events SET reason = 'x'"),
		Entry(nil, "app_user", "DELETE FROM identity_events"),
		Entry(nil, "graph_user", "SELECT count(*) FROM identity_events"),
		Entry(nil, "graph_user", "UPDATE identity_events SET reason = 'x'"),
		Entry(nil, "graph_user", "DELETE FROM identity_events"),
		Entry(nil, "tenant_admin", "SELECT count(*) FROM identity_events"),
		Entry(nil, "tenant_admin", "INSERT INTO identity_events (action, kind, tenant_id, identity_id, actor, reason, request, attestation) VALUES ('delete', 'tenant', 'idl-del', 'idl-del', 'x', 'x', '\\x7b7d', '\\x00')"),
		Entry(nil, "tenant_admin", "UPDATE identity_events SET reason = 'x'"),
		Entry(nil, "tenant_admin", "DELETE FROM identity_events"),
		Entry(nil, "tenant_admin", "SELECT public.record_identity_event('delete', 'tenant', 'idl-del', 'idl-del', 'idl-del', 'x', 'x', '\\x7b7d', '\\x00')"),
	)

	// suPool owns identity_events, so only the refusal triggers stop it.
	DescribeTable("the superuser table owner cannot change identity_events",
		func(stmt string) {
			expectRefusedUnchanged(deletedID, func() error {
				tx, err := suPool.Begin(ctx)
				Expect(err).NotTo(HaveOccurred())
				defer tx.Rollback() //nolint:errcheck // probe transaction, always rolled back
				Expect(tx.Exec(ctx, "SET LOCAL lock_timeout = '5s'")).To(Succeed())
				return tx.Exec(ctx, stmt)
			}, "42501", "append-only")
		},
		Entry(nil, "UPDATE identity_events SET reason = 'rewritten' WHERE tenant_id = '"+deletedID+"'"),
		Entry(nil, "DELETE FROM identity_events WHERE tenant_id = '"+deletedID+"'"),
		Entry(nil, "TRUNCATE identity_events"),
	)
})

// Reviewer registry (#195): tenant_admin adds, soft-deletes,
// restores, hides and unhides reviewers through SECURITY DEFINER functions
// that each append a signed identity_events row; trace_pseudonym maps a
// pseudonym back; app_user checks a reviewer only through
// require_active_reviewer and can read reviewers of its own tenant.
var _ = Describe("Reviewer registry", Ordered, func() {
	const (
		tenantID   = "idl-rev"
		otherID    = "idl-rev-other"
		goneID     = "idl-rev-gone"
		unknownID  = "idl-rev-nobody"
		alice      = "alice@example.invalid"
		bob        = "bob@example.invalid"
		stranger   = "carol@example.invalid"
		nobody     = "nobody@example.invalid"
		dave       = "dave@example.invalid" // of goneID, stays active
		erin       = "erin@example.invalid" // of goneID, deleted and hidden after the tenant is deleted
		actor      = "ops@example.invalid"
		reason     = "reviewer left the team"
		reservedID = "hidden-reviewer-0123456789abcdef"
	)
	ctx := context.Background()
	var (
		gen       attestation.Generator
		adminConn *sql.DB
		appConn   *sql.DB
		graphConn *sql.DB
	)
	connFor := func(role string) *sql.DB {
		return map[string]*sql.DB{"app_user": appConn, "graph_user": graphConn, "tenant_admin": adminConn}[role]
	}

	// reviewerSnapshot is the whole reviewers row as JSON ("" when absent),
	// read as the superuser so RLS hides nothing.
	reviewerSnapshot := func(tenant, reviewer string) string {
		var row string
		Expect(suPool.QueryRow(ctx,
			"SELECT coalesce((SELECT row_to_json(r)::text FROM reviewers r WHERE tenant_id = $1 AND reviewer_id = $2), '')",
			tenant, reviewer).Scan(&row)).To(Succeed())
		return row
	}
	reviewerField := func(tenant, reviewer, column string) sql.NullString {
		var v sql.NullString
		Expect(suPool.QueryRow(ctx,
			"SELECT (row_to_json(r) ->> $3::text) FROM reviewers r WHERE tenant_id = $1 AND reviewer_id = $2",
			tenant, reviewer, column).Scan(&v)).To(Succeed())
		return v
	}
	reviewerEvents := func(tenant, reviewer string) []string {
		rows, err := suPool.Query(ctx, `SELECT action FROM identity_events
			WHERE kind = 'reviewer' AND tenant_id = $1 AND identity_id = $2 ORDER BY event_id`, tenant, reviewer)
		Expect(err).NotTo(HaveOccurred())
		defer rows.Close()
		var actions []string
		for rows.Next() {
			var a string
			Expect(rows.Scan(&a)).To(Succeed())
			actions = append(actions, a)
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		return actions
	}
	totalEvents := func() int {
		var n int
		Expect(suPool.QueryRow(ctx, "SELECT count(*) FROM identity_events").Scan(&n)).To(Succeed())
		return n
	}

	// callReviewerFn signs a request for action and calls <action>_reviewer
	// as conn, scanning its single result into dest when dest is not nil.
	callReviewerFn := func(conn *sql.DB, action, tenant, reviewer string, dest any) error {
		req, env := signed(gen, action, "reviewer", tenant, reviewer, actor, reason)
		row := conn.QueryRowContext(ctx, "SELECT public."+action+"_reviewer($1, $2, $3, $4, $5, $6)",
			tenant, reviewer, actor, reason, req, env)
		if dest == nil {
			var ignored any
			return row.Scan(&ignored)
		}
		return row.Scan(dest)
	}
	addReviewer := func(tenant, reviewer string) (bool, error) {
		var inserted bool
		err := callReviewerFn(adminConn, "add", tenant, reviewer, &inserted)
		return inserted, err
	}
	hideReviewer := func(tenant, reviewer string) (string, error) {
		var pseudonym string
		err := callReviewerFn(adminConn, "hide", tenant, reviewer, &pseudonym)
		return pseudonym, err
	}
	// expectRefusedUnchanged runs call and asserts the SQLSTATE, a message
	// substring, and that the reviewer's row and the tenant's event count did
	// not change.
	expectRefusedUnchanged := func(tenant, reviewer string, call func() error, code, msg string) {
		before, events := reviewerSnapshot(tenant, reviewer), eventCount(tenant)
		err := call()
		Expect(err).To(HaveOccurred())
		Expect(db.IsPgErrorCode(err, code)).To(BeTrue(), "want SQLSTATE %s, got %v", code, err)
		Expect(err.Error()).To(ContainSubstring(msg))
		Expect(reviewerSnapshot(tenant, reviewer)).To(Equal(before))
		Expect(eventCount(tenant)).To(Equal(events))
	}
	// inTenantAsAppUser enters tenant as app_user, runs stmt and always
	// rolls back, so a statement that unexpectedly succeeds changes nothing.
	inTenantAsAppUser := func(tenant, stmt string, args ...any) error {
		tx, err := appConn.BeginTx(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		defer tx.Rollback() //nolint:errcheck // probe transaction, always rolled back
		_, err = tx.ExecContext(ctx, "SELECT public.enter_tenant($1)", tenant)
		Expect(err).NotTo(HaveOccurred())
		_, err = tx.ExecContext(ctx, stmt, args...)
		return err
	}
	trace := func(pseudonym string) (kind, tenant, identity, status string, hidden bool, err error) {
		req, env := signed(gen, "trace", "", "", pseudonym, actor, reason)
		err = adminConn.QueryRowContext(ctx,
			"SELECT kind, tenant_id, identity_id, status, hidden FROM public.trace_pseudonym($1, $2, $3, $4, $5)",
			pseudonym, actor, reason, req, env).Scan(&kind, &tenant, &identity, &status, &hidden)
		return kind, tenant, identity, status, hidden, err
	}

	BeforeAll(func() {
		if suPool == nil {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}
		kp, err := attestation.NewEphemeralKeyProvider()
		Expect(err).NotTo(HaveOccurred())
		gen, err = attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())
		adminConn, appConn, graphConn = tenantAdminConn(), appUserConn(), graphUserConn()
		for _, id := range []string{tenantID, otherID, goneID} {
			setupTenant(id, "Reviewer Registry "+id)
		}
		for _, r := range []string{dave, erin} {
			_, err = addReviewer(goneID, r)
			Expect(err).NotTo(HaveOccurred())
		}
		req, env := signed(gen, "delete", "tenant", goneID, goneID, actor, reason)
		_, err = adminConn.ExecContext(ctx, "SELECT public.delete_tenant($1, $2, $3, $4, $5)", goneID, actor, reason, req, env)
		Expect(err).NotTo(HaveOccurred())
		Expect(suPool.Exec(ctx, "INSERT INTO reviewers (tenant_id, reviewer_id, created_by) VALUES ($1, $2, 'test')",
			otherID, stranger)).To(Succeed())
	})

	AfterAll(func() {
		if suPool != nil {
			for _, id := range []string{tenantID, otherID, goneID} {
				stmt, err := dbtest.HardDeleteTenantSQL(id)
				Expect(err).NotTo(HaveOccurred())
				Expect(suPool.Exec(ctx, stmt)).To(Succeed())
			}
		}
	})

	DescribeTable("new functions are not executable by PUBLIC",
		func(signature string) {
			var ok bool
			Expect(suPool.QueryRow(ctx, "SELECT has_function_privilege('public', $1, 'EXECUTE')", signature).Scan(&ok)).To(Succeed())
			Expect(ok).To(BeFalse(), "PUBLIC must not hold EXECUTE on %s", signature)
		},
		Entry(nil, "public.add_reviewer(text, text, text, text, bytea, bytea)"),
		Entry(nil, "public.delete_reviewer(text, text, text, text, bytea, bytea)"),
		Entry(nil, "public.restore_reviewer(text, text, text, text, bytea, bytea)"),
		Entry(nil, "public.hide_reviewer(text, text, text, text, bytea, bytea)"),
		Entry(nil, "public.unhide_reviewer(text, text, text, text, bytea, bytea)"),
		Entry(nil, "public.list_reviewers(text)"),
		Entry(nil, "public.trace_pseudonym(text, text, text, bytea, bytea)"),
		Entry(nil, "public.require_active_reviewer(text)"),
	)

	It("declares every reviewer function SECURITY DEFINER with a pinned search_path", func() {
		rows, err := suPool.Query(ctx, `
			SELECT proname, prosecdef, coalesce(array_to_string(proconfig, ';'), '')
			FROM pg_proc
			WHERE pronamespace = 'public'::regnamespace
			  AND proname IN ('add_reviewer', 'delete_reviewer', 'restore_reviewer', 'hide_reviewer',
			                  'unhide_reviewer', 'list_reviewers', 'trace_pseudonym', 'require_active_reviewer')`)
		Expect(err).NotTo(HaveOccurred())
		defer rows.Close()
		var names []string
		for rows.Next() {
			var name, config string
			var definer bool
			Expect(rows.Scan(&name, &definer, &config)).To(Succeed())
			Expect(definer).To(BeTrue(), name)
			Expect(config).To(Equal("search_path=pg_catalog, public, pg_temp"), name)
			names = append(names, name)
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		Expect(names).To(HaveLen(8))
	})

	It("adds a reviewer and records a signed event", func() {
		inserted, err := addReviewer(tenantID, alice)
		Expect(err).NotTo(HaveOccurred())
		Expect(inserted).To(BeTrue())
		Expect(reviewerField(tenantID, alice, "status").String).To(Equal("active"))
		Expect(reviewerField(tenantID, alice, "created_by").String).To(Equal(actor))
		Expect(reviewerEvents(tenantID, alice)).To(Equal([]string{"add"}))

		inserted, err = addReviewer(tenantID, bob)
		Expect(err).NotTo(HaveOccurred())
		Expect(inserted).To(BeTrue())
	})

	It("adding an active reviewer again changes nothing but is still recorded", func() {
		before := reviewerSnapshot(tenantID, alice)
		inserted, err := addReviewer(tenantID, alice)
		Expect(err).NotTo(HaveOccurred())
		Expect(inserted).To(BeFalse())
		Expect(reviewerSnapshot(tenantID, alice)).To(Equal(before))
		Expect(reviewerEvents(tenantID, alice)).To(Equal([]string{"add", "add"}))
	})

	DescribeTable("add_reviewer refuses and changes nothing",
		func(tenant, reviewer, code, wantMsg string) {
			expectRefusedUnchanged(tenant, reviewer, func() error {
				_, err := addReviewer(tenant, reviewer)
				return err
			}, code, wantMsg)
			Expect(reviewerSnapshot(tenant, reviewer)).To(BeEmpty())
		},
		Entry("deleted tenant", goneID, alice, "55000", "restore the tenant first"),
		Entry("unknown tenant", unknownID, alice, "P0002", "does not exist"),
		Entry("empty reviewer ID", tenantID, "", "22023", "reviewer ID must be 1-256 bytes"),
		Entry("257-byte reviewer ID", tenantID, strings.Repeat("r", 257), "22023", "reviewer ID must be 1-256 bytes"),
		Entry("reviewer ID with a control character", tenantID, "a\x01", "22023", "reviewer ID must be 1-256 bytes"),
		Entry("reviewer ID with a leading space", tenantID, " a", "22023", "reviewer ID must be 1-256 bytes"),
		Entry("pseudonym-shaped reviewer ID", tenantID, reservedID, "22023", "reserved for pseudonyms of hidden reviewers"),
	)

	It("accepts a 256-byte reviewer ID, the longest allowed", func() {
		inserted, err := addReviewer(otherID, strings.Repeat("r", 256))
		Expect(err).NotTo(HaveOccurred())
		Expect(inserted).To(BeTrue())
	})

	It("refuses a pseudonym-shaped reviewer ID inserted directly", func() {
		expectRefusedUnchanged(tenantID, reservedID, func() error {
			return suPool.Exec(ctx, "INSERT INTO reviewers (tenant_id, reviewer_id, created_by) VALUES ($1, $2, 'x')",
				tenantID, reservedID)
		}, "23514", "reviewers_id_not_reserved")
	})

	DescribeTable("refuses reviewer actions by a non-admin role",
		func(role, action string) {
			expectRefusedUnchanged(tenantID, alice, func() error {
				return callReviewerFn(connFor(role), action, tenantID, alice, nil)
			}, "42501", "permission denied")
		},
		Entry(nil, "app_user", "add"),
		Entry(nil, "app_user", "delete"),
		Entry(nil, "app_user", "restore"),
		Entry(nil, "app_user", "hide"),
		Entry(nil, "app_user", "unhide"),
		Entry(nil, "graph_user", "add"),
		Entry(nil, "graph_user", "delete"),
		Entry(nil, "graph_user", "restore"),
		Entry(nil, "graph_user", "hide"),
		Entry(nil, "graph_user", "unhide"),
	)

	DescribeTable("refuses actions on an unregistered reviewer",
		func(action string) {
			expectRefusedUnchanged(tenantID, nobody, func() error {
				return callReviewerFn(adminConn, action, tenantID, nobody, nil)
			}, "P0002", "crosscodexd admin reviewer add")
			Expect(reviewerSnapshot(tenantID, nobody)).To(BeEmpty())
		},
		Entry(nil, "delete"),
		Entry(nil, "restore"),
		Entry(nil, "hide"),
		Entry(nil, "unhide"),
	)

	DescribeTable("refuses actions that do not apply to an active reviewer",
		func(action, wantMsg string) {
			expectRefusedUnchanged(tenantID, alice, func() error {
				return callReviewerFn(adminConn, action, tenantID, alice, nil)
			}, "55000", wantMsg)
		},
		Entry(nil, "restore", "is not deleted"),
		Entry(nil, "hide", "only a deleted reviewer can be hidden"),
		Entry(nil, "unhide", "is not hidden"),
	)

	It("soft-deletes a reviewer", func() {
		Expect(callReviewerFn(adminConn, "delete", tenantID, alice, nil)).To(Succeed())
		Expect(reviewerField(tenantID, alice, "status").String).To(Equal("deleted"))
		Expect(reviewerField(tenantID, alice, "deleted_by").String).To(Equal(actor))
		Expect(reviewerField(tenantID, alice, "deleted_at").Valid).To(BeTrue())
		Expect(reviewerEvents(tenantID, alice)).To(Equal([]string{"add", "add", "delete"}))
	})

	DescribeTable("refuses actions that do not apply to a deleted reviewer",
		func(action, wantMsg string) {
			expectRefusedUnchanged(tenantID, alice, func() error {
				return callReviewerFn(adminConn, action, tenantID, alice, nil)
			}, "55000", wantMsg)
		},
		Entry(nil, "add", "admin reviewer restore"),
		Entry(nil, "delete", "already deleted"),
		Entry(nil, "unhide", "is not hidden"),
	)

	DescribeTable("require_active_reviewer as app_user in the reviewer's tenant",
		func(reviewer, code, wantMsg string) {
			before, events := reviewerSnapshot(tenantID, reviewer), eventCount(tenantID)
			err := inTenantAsAppUser(tenantID, "SELECT public.require_active_reviewer($1)", reviewer)
			if code == "" {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(err).To(HaveOccurred())
				Expect(db.IsPgErrorCode(err, code)).To(BeTrue(), "want SQLSTATE %s, got %v", code, err)
				Expect(err.Error()).To(ContainSubstring(wantMsg))
			}
			Expect(reviewerSnapshot(tenantID, reviewer)).To(Equal(before))
			Expect(eventCount(tenantID)).To(Equal(events))
		},
		Entry("active reviewer", bob, "", ""),
		Entry("unregistered reviewer", nobody, "P0002", "not registered"),
		Entry("deleted reviewer", alice, "55000", "deleted"),
		Entry("reviewer of another tenant", stranger, "P0002", "not registered"),
	)

	It("require_active_reviewer refuses a deleted tenant even when set_config bypasses enter_tenant", func() {
		before, events := reviewerSnapshot(goneID, dave), eventCount(goneID)
		tx, err := appConn.BeginTx(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		defer tx.Rollback() //nolint:errcheck // probe transaction, always rolled back
		_, err = tx.ExecContext(ctx, "SELECT set_config('app.current_tenant', $1, true)", goneID)
		Expect(err).NotTo(HaveOccurred())
		_, err = tx.ExecContext(ctx, "SELECT public.require_active_reviewer($1)", dave)
		Expect(err).To(HaveOccurred())
		Expect(db.IsPgErrorCode(err, "42501")).To(BeTrue(), "got %v", err)
		Expect(err.Error()).To(ContainSubstring("admin tenant restore"))
		Expect(reviewerSnapshot(goneID, dave)).To(Equal(before))
		Expect(eventCount(goneID)).To(Equal(events))
	})

	// The lock is what keeps a reviewer from being deleted while a verdict
	// by them is being recorded. lock_timeout makes the wait deterministic.
	It("require_active_reviewer holds the reviewer row until the caller's transaction ends", func() {
		deleteBob := func() error {
			tx, err := adminConn.BeginTx(ctx, nil)
			Expect(err).NotTo(HaveOccurred())
			defer tx.Rollback() //nolint:errcheck // the successful delete is undone too, so bob stays active
			_, err = tx.ExecContext(ctx, "SET LOCAL lock_timeout = '500ms'")
			Expect(err).NotTo(HaveOccurred())
			req, env := signed(gen, "delete", "reviewer", tenantID, bob, actor, reason)
			_, err = tx.ExecContext(ctx, "SELECT public.delete_reviewer($1, $2, $3, $4, $5, $6)",
				tenantID, bob, actor, reason, req, env)
			return err
		}

		decide, err := appConn.BeginTx(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		defer decide.Rollback() //nolint:errcheck // ended explicitly below; this covers a failed assertion
		_, err = decide.ExecContext(ctx, "SELECT public.enter_tenant($1)", tenantID)
		Expect(err).NotTo(HaveOccurred())
		_, err = decide.ExecContext(ctx, "SELECT public.require_active_reviewer($1)", bob)
		Expect(err).NotTo(HaveOccurred())

		before, events := reviewerSnapshot(tenantID, bob), eventCount(tenantID)
		err = deleteBob()
		Expect(err).To(HaveOccurred())
		Expect(db.IsPgErrorCode(err, "55P03")).To(BeTrue(), "want lock_not_available, got %v", err)
		Expect(reviewerSnapshot(tenantID, bob)).To(Equal(before))
		Expect(eventCount(tenantID)).To(Equal(events))

		Expect(decide.Rollback()).To(Succeed())
		Expect(deleteBob()).To(Succeed())
		Expect(reviewerField(tenantID, bob, "status").String).To(Equal("active"))
	})

	It("require_active_reviewer refuses a transaction without a tenant", func() {
		before := reviewerSnapshot(tenantID, bob)
		_, err := appConn.ExecContext(ctx, "SELECT public.require_active_reviewer($1)", bob)
		Expect(err).To(HaveOccurred())
		Expect(db.IsPgErrorCode(err, "42501")).To(BeTrue(), "got %v", err)
		Expect(err.Error()).To(ContainSubstring("enter_tenant"))
		Expect(reviewerSnapshot(tenantID, bob)).To(Equal(before))
	})

	var reviewerPseudonym, tenantPseudonym string

	It("hides a deleted reviewer behind a pseudonym in list_reviewers", func() {
		var err error
		reviewerPseudonym, err = hideReviewer(tenantID, alice)
		Expect(err).NotTo(HaveOccurred())
		Expect(reviewerPseudonym).To(MatchRegexp(`^hidden-reviewer-[0-9a-f]{16}$`))

		rows, err := adminConn.QueryContext(ctx, "SELECT reviewer_id, status, hidden FROM public.list_reviewers($1)", tenantID)
		Expect(err).NotTo(HaveOccurred())
		defer rows.Close()
		listed := map[string]string{}
		for rows.Next() {
			var id, status string
			var hidden bool
			Expect(rows.Scan(&id, &status, &hidden)).To(Succeed())
			listed[id] = fmt.Sprintf("%s hidden=%t", status, hidden)
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		Expect(listed).To(Equal(map[string]string{
			reviewerPseudonym: "deleted hidden=true",
			bob:               "active hidden=false",
		}))
		Expect(reviewerEvents(tenantID, alice)).To(Equal([]string{"add", "add", "delete", "hide"}))
	})

	DescribeTable("refuses actions that do not apply to a hidden reviewer",
		func(action, wantMsg string) {
			expectRefusedUnchanged(tenantID, alice, func() error {
				return callReviewerFn(adminConn, action, tenantID, alice, nil)
			}, "55000", wantMsg)
		},
		Entry(nil, "hide", "already hidden"),
		Entry(nil, "restore", "unhide"),
	)

	DescribeTable("trace_pseudonym resolves a pseudonym and records a trace event",
		func(pseudonym func() string, wantKind, wantTenant, wantIdentity string) {
			p := pseudonym()
			events := eventCount(wantTenant)
			kind, tenant, identity, status, hidden, err := trace(p)
			Expect(err).NotTo(HaveOccurred())
			Expect([]string{kind, tenant, identity, status}).To(Equal([]string{wantKind, wantTenant, wantIdentity, "deleted"}))
			Expect(hidden).To(BeTrue())
			Expect(eventCount(wantTenant)).To(Equal(events + 1))
			var action, gotKind, gotIdentity string
			Expect(suPool.QueryRow(ctx, `SELECT action, kind, identity_id FROM identity_events
				WHERE tenant_id = $1 ORDER BY event_id DESC LIMIT 1`, wantTenant).Scan(&action, &gotKind, &gotIdentity)).To(Succeed())
			Expect([]string{action, gotKind, gotIdentity}).To(Equal([]string{"trace", wantKind, wantIdentity}))
		},
		Entry("reviewer", func() string { return reviewerPseudonym }, "reviewer", tenantID, alice),
		Entry("tenant", func() string {
			if tenantPseudonym == "" {
				req, env := signed(gen, "hide", "tenant", goneID, goneID, actor, reason)
				Expect(adminConn.QueryRowContext(ctx, "SELECT public.hide_tenant($1, $2, $3, $4, $5)",
					goneID, actor, reason, req, env).Scan(&tenantPseudonym)).To(Succeed())
			}
			return tenantPseudonym
		}, "tenant", goneID, goneID),
	)

	It("trace_pseudonym refuses an unknown pseudonym and records nothing", func() {
		events := totalEvents()
		_, _, _, _, _, err := trace("hidden-reviewer-ffffffffffffffff")
		Expect(err).To(HaveOccurred())
		Expect(db.IsPgErrorCode(err, "P0002")).To(BeTrue(), "got %v", err)
		Expect(err.Error()).To(ContainSubstring("no tenant or reviewer has pseudonym"))
		Expect(totalEvents()).To(Equal(events))
	})

	It("trace_pseudonym refuses a request signed for another pseudonym and records nothing", func() {
		events := totalEvents()
		req, env := signed(gen, "trace", "", "", reviewerPseudonym, actor, reason)
		_, err := adminConn.ExecContext(ctx, "SELECT * FROM public.trace_pseudonym($1, $2, $3, $4, $5)",
			tenantPseudonym, actor, reason, req, env)
		Expect(err).To(HaveOccurred())
		Expect(db.IsPgErrorCode(err, "22023")).To(BeTrue(), "got %v", err)
		Expect(err.Error()).To(ContainSubstring("its identity_id differs"))
		Expect(totalEvents()).To(Equal(events))
	})

	DescribeTable("trace_pseudonym refuses a non-admin role and records nothing",
		func(role string, pseudonym func() string, target func() string) {
			p := pseudonym()
			Expect(p).NotTo(BeEmpty())
			before, events := target(), totalEvents()
			req, env := signed(gen, "trace", "", "", p, actor, reason)
			_, err := connFor(role).ExecContext(ctx, "SELECT * FROM public.trace_pseudonym($1, $2, $3, $4, $5)",
				p, actor, reason, req, env)
			Expect(err).To(HaveOccurred())
			Expect(db.IsPgErrorCode(err, "42501")).To(BeTrue(), "want insufficient_privilege, got %v", err)
			Expect(err.Error()).To(ContainSubstring("permission denied"))
			Expect(target()).To(Equal(before))
			Expect(totalEvents()).To(Equal(events))
		},
		Entry("app_user, reviewer pseudonym", "app_user",
			func() string { return reviewerPseudonym }, func() string { return reviewerSnapshot(tenantID, alice) }),
		Entry("graph_user, reviewer pseudonym", "graph_user",
			func() string { return reviewerPseudonym }, func() string { return reviewerSnapshot(tenantID, alice) }),
		Entry("app_user, tenant pseudonym", "app_user",
			func() string { return tenantPseudonym }, func() string { return tenantSnapshot(goneID) }),
		Entry("graph_user, tenant pseudonym", "graph_user",
			func() string { return tenantPseudonym }, func() string { return tenantSnapshot(goneID) }),
	)

	It("refuses a reviewer request signed for another tenant, replayed", func() {
		const zed = "zed@example.invalid"
		req, env := signed(gen, "add", "reviewer", tenantID, zed, actor, reason)
		expectRefusedUnchanged(otherID, zed, func() error {
			_, err := adminConn.ExecContext(ctx, "SELECT public.add_reviewer($1, $2, $3, $4, $5, $6)",
				otherID, zed, actor, reason, req, env)
			return err
		}, "22023", "its tenant_id differs")
		Expect(reviewerSnapshot(otherID, zed)).To(BeEmpty())
	})

	// A deleted tenant's reviewers can still be deleted and concealed, but
	// not brought back: restore the tenant first.
	It("deletes and hides a reviewer of a deleted tenant", func() {
		Expect(callReviewerFn(adminConn, "delete", goneID, erin, nil)).To(Succeed())
		Expect(reviewerField(goneID, erin, "status").String).To(Equal("deleted"))
		pseudonym, err := hideReviewer(goneID, erin)
		Expect(err).NotTo(HaveOccurred())
		Expect(pseudonym).To(MatchRegexp(`^hidden-reviewer-[0-9a-f]{16}$`))
		Expect(callReviewerFn(adminConn, "unhide", goneID, erin, nil)).To(Succeed())
		Expect(reviewerEvents(goneID, erin)).To(Equal([]string{"add", "delete", "hide", "unhide"}))
	})

	It("refuses to restore a reviewer of a deleted tenant", func() {
		expectRefusedUnchanged(goneID, erin, func() error {
			return callReviewerFn(adminConn, "restore", goneID, erin, nil)
		}, "55000", "restore the tenant first")
		Expect(reviewerField(goneID, erin, "status").String).To(Equal("deleted"))
	})

	It("unhides, hides again with the same pseudonym, and restores the reviewer as active", func() {
		Expect(callReviewerFn(adminConn, "unhide", tenantID, alice, nil)).To(Succeed())
		Expect(reviewerField(tenantID, alice, "hidden_at").Valid).To(BeFalse())
		var listed bool
		Expect(adminConn.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT 1 FROM public.list_reviewers($1) WHERE reviewer_id = $2 AND NOT hidden)",
			tenantID, alice).Scan(&listed)).To(Succeed())
		Expect(listed).To(BeTrue())

		again, err := hideReviewer(tenantID, alice)
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(Equal(reviewerPseudonym))
		Expect(callReviewerFn(adminConn, "unhide", tenantID, alice, nil)).To(Succeed())

		Expect(callReviewerFn(adminConn, "restore", tenantID, alice, nil)).To(Succeed())
		Expect(reviewerField(tenantID, alice, "status").String).To(Equal("active"))
		Expect(reviewerField(tenantID, alice, "deleted_at").Valid).To(BeFalse())
		Expect(reviewerField(tenantID, alice, "deleted_by").Valid).To(BeFalse())
		Expect(reviewerField(tenantID, alice, "pseudonym").String).To(Equal(reviewerPseudonym))
		Expect(reviewerEvents(tenantID, alice)).To(Equal(
			[]string{"add", "add", "delete", "hide", "trace", "unhide", "hide", "unhide", "restore"}))
		Expect(inTenantAsAppUser(tenantID, "SELECT public.require_active_reviewer($1)", alice)).To(Succeed())
	})

	It("app_user sees only its own tenant's reviewers", func() {
		tx, err := appConn.BeginTx(ctx, nil)
		Expect(err).NotTo(HaveOccurred())
		defer tx.Rollback() //nolint:errcheck // read-only probe
		_, err = tx.ExecContext(ctx, "SELECT public.enter_tenant($1)", tenantID)
		Expect(err).NotTo(HaveOccurred())
		var total, foreign int
		Expect(tx.QueryRowContext(ctx,
			"SELECT count(*), count(*) FILTER (WHERE tenant_id <> $1) FROM reviewers", tenantID).Scan(&total, &foreign)).To(Succeed())
		Expect(total).To(Equal(2)) // alice, bob
		Expect(foreign).To(BeZero())
	})

	DescribeTable("no application role can change reviewers directly",
		func(role, stmt string) {
			expectRefusedUnchanged(tenantID, alice, func() error {
				if role == "app_user" {
					return inTenantAsAppUser(tenantID, stmt)
				}
				_, err := connFor(role).ExecContext(ctx, stmt)
				return err
			}, "42501", "permission denied")
		},
		Entry(nil, "app_user", "INSERT INTO reviewers (tenant_id, reviewer_id, created_by) VALUES ('idl-rev', 'mallory', 'x')"),
		Entry(nil, "app_user", "UPDATE reviewers SET status = 'deleted', deleted_at = now(), deleted_by = 'x'"),
		Entry(nil, "app_user", "DELETE FROM reviewers"),
		Entry(nil, "app_user", "TRUNCATE reviewers"),
		Entry(nil, "tenant_admin", "SELECT count(*) FROM reviewers"),
		Entry(nil, "tenant_admin", "INSERT INTO reviewers (tenant_id, reviewer_id, created_by) VALUES ('idl-rev', 'mallory', 'x')"),
		Entry(nil, "tenant_admin", "UPDATE reviewers SET status = 'active'"),
		Entry(nil, "tenant_admin", "DELETE FROM reviewers"),
	)

	// suPool owns reviewers, so only the refusal triggers stop it.
	DescribeTable("the superuser table owner cannot delete reviewers",
		func(stmt string) {
			expectRefusedUnchanged(tenantID, alice, func() error {
				tx, err := suPool.Begin(ctx)
				Expect(err).NotTo(HaveOccurred())
				defer tx.Rollback() //nolint:errcheck // probe transaction, always rolled back
				Expect(tx.Exec(ctx, "SET LOCAL lock_timeout = '5s'")).To(Succeed())
				return tx.Exec(ctx, stmt)
			}, "42501", "never deleted")
		},
		Entry(nil, "DELETE FROM reviewers WHERE tenant_id = '"+tenantID+"'"),
		Entry(nil, "TRUNCATE reviewers"),
	)
})

// TenantAdmin's identity methods (#195) are the Go API the CLI calls. These
// specs run them as tenant_admin, signing each request with an ephemeral
// key, and check that every refusal maps to its sentinel
// and leaves the rows and the audit log unchanged.
var _ = Describe("TenantAdmin identity API", Ordered, func() {
	const (
		tenantID = "idl-go"
		unknown  = "idl-go-nobody"
		rita     = "rita@example.invalid"
		nobody   = "nobody@example.invalid"
		actor    = "ops@example.invalid"
		reason   = "identity API round trip"
	)
	ctx := context.Background()
	var (
		gen   attestation.Generator
		admin *db.TenantAdmin
	)
	sign := func(action, kind, tenant, identity string) db.SignedRequest {
		return signedRequest(gen, action, kind, tenant, identity, actor, reason)
	}
	// snapshot is the tenant's row, its reviewers and its identity_events
	// count, read as the superuser so RLS hides nothing.
	snapshot := func(id string) string {
		var row string
		Expect(suPool.QueryRow(ctx, `SELECT coalesce((SELECT row_to_json(t)::text FROM tenants t WHERE tenant_id = $1), '')
			|| coalesce((SELECT json_agg(r ORDER BY reviewer_id)::text FROM reviewers r WHERE tenant_id = $1), '')`, id).
			Scan(&row)).To(Succeed())
		return fmt.Sprintf("%s events=%d", row, eventCount(id))
	}
	totalEvents := func() int {
		var n int
		Expect(suPool.QueryRow(ctx, "SELECT count(*) FROM identity_events").Scan(&n)).To(Succeed())
		return n
	}
	// expectRefusedUnchanged asserts the sentinel, an actionable message and
	// that nothing changed, the audit log included.
	expectRefusedUnchanged := func(call func() error, sentinel error, msg string) {
		before, events := snapshot(tenantID), totalEvents()
		err := call()
		Expect(err).To(HaveOccurred())
		if sentinel != nil {
			Expect(errors.Is(err, sentinel)).To(BeTrue(), "want %v, got %v", sentinel, err)
		}
		Expect(err.Error()).To(ContainSubstring(msg))
		Expect(snapshot(tenantID)).To(Equal(before))
		Expect(totalEvents()).To(Equal(events))
	}

	BeforeAll(func() {
		if suPool == nil {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}
		kp, err := attestation.NewEphemeralKeyProvider()
		Expect(err).NotTo(HaveOccurred())
		gen, err = attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())
		admin, err = db.OpenTenantAdmin(tenantAdminDSN())
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(admin.Close)
		setupTenant(tenantID, "Identity API")
	})

	AfterAll(func() {
		if suPool != nil {
			stmt, err := dbtest.HardDeleteTenantSQL(tenantID)
			Expect(err).NotTo(HaveOccurred())
			Expect(suPool.Exec(ctx, stmt)).To(Succeed())
		}
	})

	It("adds, deletes, hides, traces, unhides and restores a reviewer", func() {
		created, err := admin.AddReviewer(ctx, tenantID, rita, actor, reason, sign("add", db.IdentityKindReviewer, tenantID, rita))
		Expect(err).NotTo(HaveOccurred())
		Expect(created).To(BeTrue())
		created, err = admin.AddReviewer(ctx, tenantID, rita, actor, reason, sign("add", db.IdentityKindReviewer, tenantID, rita))
		Expect(err).NotTo(HaveOccurred())
		Expect(created).To(BeFalse())

		Expect(admin.DeleteReviewer(ctx, tenantID, rita, actor, reason, sign("delete", db.IdentityKindReviewer, tenantID, rita))).To(Succeed())
		pseudonym, err := admin.HideReviewer(ctx, tenantID, rita, actor, reason, sign("hide", db.IdentityKindReviewer, tenantID, rita))
		Expect(err).NotTo(HaveOccurred())
		Expect(db.ValidatePseudonym(pseudonym)).To(Succeed())
		Expect(pseudonym).To(HavePrefix("hidden-reviewer-"))

		list, err := admin.ListReviewers(ctx, tenantID)
		Expect(err).NotTo(HaveOccurred())
		Expect(list).To(HaveLen(1))
		Expect(list[0].ID).To(Equal(pseudonym))
		Expect(list[0].Status).To(Equal("deleted"))
		Expect(list[0].Hidden).To(BeTrue())
		Expect(list[0].DeletedAt).NotTo(BeNil())
		Expect(list[0].CreatedBy).To(Equal(actor))
		Expect(list[0].CreatedAt).NotTo(BeZero())

		traced, err := admin.Trace(ctx, pseudonym, actor, reason, sign("trace", "", "", pseudonym))
		Expect(err).NotTo(HaveOccurred())
		Expect(traced).To(Equal(db.TraceResult{Kind: db.IdentityKindReviewer, TenantID: tenantID, IdentityID: rita,
			Status: "deleted", Hidden: true}))

		Expect(admin.UnhideReviewer(ctx, tenantID, rita, actor, reason, sign("unhide", db.IdentityKindReviewer, tenantID, rita))).To(Succeed())
		Expect(admin.RestoreReviewer(ctx, tenantID, rita, actor, reason, sign("restore", db.IdentityKindReviewer, tenantID, rita))).To(Succeed())
		list, err = admin.ListReviewers(ctx, tenantID)
		Expect(err).NotTo(HaveOccurred())
		Expect(list).To(ConsistOf(SatisfyAll(
			HaveField("ID", rita), HaveField("Status", "active"),
			HaveField("Hidden", false), HaveField("DeletedAt", BeNil()))))
	})

	It("deletes, hides, traces, unhides and restores a tenant", func() {
		Expect(admin.DeleteTenant(ctx, tenantID, actor, reason, sign("delete", db.IdentityKindTenant, tenantID, tenantID))).To(Succeed())
		rec, err := admin.Get(ctx, tenantID)
		Expect(err).NotTo(HaveOccurred())
		Expect(rec.Status).To(Equal(db.TenantStatusDeleted))
		Expect(rec.DeletedAt).NotTo(BeNil())
		Expect(rec.Hidden).To(BeFalse())

		pseudonym, err := admin.HideTenant(ctx, tenantID, actor, reason, sign("hide", db.IdentityKindTenant, tenantID, tenantID))
		Expect(err).NotTo(HaveOccurred())
		Expect(pseudonym).To(HavePrefix("hidden-tenant-"))
		Expect(db.ValidatePseudonym(pseudonym)).To(Succeed())

		_, err = admin.Get(ctx, tenantID)
		Expect(err).To(MatchError(db.ErrTenantNotFound))
		Expect(err.Error()).To(ContainSubstring("crosscodexd admin tenant unhide"))
		list, err := admin.List(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(list).NotTo(ContainElement(HaveField("ID", tenantID)))
		Expect(list).To(ContainElement(SatisfyAll(
			HaveField("ID", pseudonym), HaveField("DisplayName", ""),
			HaveField("Status", db.TenantStatusDeleted), HaveField("Hidden", true),
			HaveField("DeletedAt", Not(BeNil())))))

		traced, err := admin.Trace(ctx, pseudonym, actor, reason, sign("trace", "", "", pseudonym))
		Expect(err).NotTo(HaveOccurred())
		Expect(traced).To(Equal(db.TraceResult{Kind: db.IdentityKindTenant, TenantID: tenantID, IdentityID: tenantID,
			Status: db.TenantStatusDeleted, Hidden: true}))

		Expect(admin.UnhideTenant(ctx, tenantID, actor, reason, sign("unhide", db.IdentityKindTenant, tenantID, tenantID))).To(Succeed())
		Expect(admin.RestoreTenant(ctx, tenantID, actor, reason, sign("restore", db.IdentityKindTenant, tenantID, tenantID))).To(Succeed())
		rec, err = admin.Get(ctx, tenantID)
		Expect(err).NotTo(HaveOccurred())
		Expect(rec).To(SatisfyAll(
			HaveField("ID", tenantID), HaveField("DisplayName", "Identity API"),
			HaveField("Status", db.TenantStatusSuspended), HaveField("Hidden", false), HaveField("DeletedAt", BeNil())))

		// A restored tenant comes back suspended, so it is re-activated deliberately.
		previous, err := admin.SetStatus(ctx, tenantID, db.TenantStatusActive)
		Expect(err).NotTo(HaveOccurred())
		Expect(previous).To(Equal(db.TenantStatusSuspended))
	})

	It("records one event per change and trace, in order", func() {
		rows, err := suPool.Query(ctx, "SELECT action, kind FROM identity_events WHERE tenant_id = $1 ORDER BY event_id", tenantID)
		Expect(err).NotTo(HaveOccurred())
		defer rows.Close()
		var got []string
		for rows.Next() {
			var action, kind string
			Expect(rows.Scan(&action, &kind)).To(Succeed())
			got = append(got, kind+" "+action)
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		// The second add of an active reviewer changes nothing but is still recorded.
		Expect(got).To(Equal([]string{
			"reviewer add", "reviewer add", "reviewer delete", "reviewer hide", "reviewer trace",
			"reviewer unhide", "reviewer restore",
			"tenant delete", "tenant hide", "tenant trace", "tenant unhide", "tenant restore",
		}))
	})

	DescribeTable("refuses with the sentinel, an actionable message and nothing changed",
		func(call func() error, sentinel error, msg string) { expectRefusedUnchanged(call, sentinel, msg) },
		Entry("delete an unknown tenant", func() error {
			return admin.DeleteTenant(ctx, unknown, actor, reason, sign("delete", db.IdentityKindTenant, unknown, unknown))
		}, db.ErrTenantNotFound, "crosscodexd admin tenant list"),
		Entry("hide an active tenant", func() error {
			_, err := admin.HideTenant(ctx, tenantID, actor, reason, sign("hide", db.IdentityKindTenant, tenantID, tenantID))
			return err
		}, db.ErrIdentityState, "crosscodexd admin tenant delete"),
		Entry("restore an active tenant", func() error {
			return admin.RestoreTenant(ctx, tenantID, actor, reason, sign("restore", db.IdentityKindTenant, tenantID, tenantID))
		}, db.ErrIdentityState, "is not deleted"),
		Entry("unhide a shown tenant", func() error {
			return admin.UnhideTenant(ctx, tenantID, actor, reason, sign("unhide", db.IdentityKindTenant, tenantID, tenantID))
		}, db.ErrIdentityState, "is not hidden"),
		Entry("add a reviewer to an unknown tenant", func() error {
			_, err := admin.AddReviewer(ctx, unknown, rita, actor, reason, sign("add", db.IdentityKindReviewer, unknown, rita))
			return err
		}, db.ErrTenantNotFound, "does not exist"),
		Entry("delete an unregistered reviewer", func() error {
			return admin.DeleteReviewer(ctx, tenantID, nobody, actor, reason, sign("delete", db.IdentityKindReviewer, tenantID, nobody))
		}, db.ErrReviewerNotFound, "crosscodexd admin reviewer add"),
		Entry("hide an active reviewer", func() error {
			_, err := admin.HideReviewer(ctx, tenantID, rita, actor, reason, sign("hide", db.IdentityKindReviewer, tenantID, rita))
			return err
		}, db.ErrIdentityState, "crosscodexd admin reviewer delete"),
		Entry("trace an unknown pseudonym", func() error {
			p := "hidden-tenant-0000000000000000"
			_, err := admin.Trace(ctx, p, actor, reason, sign("trace", "", "", p))
			return err
		}, db.ErrPseudonymNotFound, "Check the pseudonym"),
		Entry("replay a request signed for another tenant", func() error {
			return admin.DeleteTenant(ctx, tenantID, actor, reason, sign("delete", db.IdentityKindTenant, unknown, unknown))
		}, nil, "Sign a new request for this change"),
		Entry("add a reserved reviewer ID", func() error {
			r := "hidden-reviewer-0123456789abcdef"
			_, err := admin.AddReviewer(ctx, tenantID, r, actor, reason, sign("add", db.IdentityKindReviewer, tenantID, r))
			return err
		}, nil, "Choose another ID"),
		Entry("provision a reserved tenant ID", func() error {
			_, err := admin.Provision(ctx, "hidden-tenant-0123456789abcdef", "Reserved")
			return err
		}, tenant.ErrInvalidTenant, "choose another ID"),
	)
})

// export_identity_events is the one read path for the audit log, used by
// 'crosscodexd admin identity export' before a database is decommissioned.
// These specs write events through the real identity functions and read
// them back as tenant_admin through TenantAdmin.ExportIdentityEvents.
var _ = Describe("Identity audit log export", Ordered, func() {
	const (
		tenantID = "idl-export"
		rita     = "rita-export@example.invalid"
		actor    = "ops@example.invalid"
		reason   = "decommission export"
	)
	ctx := context.Background()
	var (
		gen                           attestation.Generator
		admin                         *db.TenantAdmin
		adminConn, appConn, graphConn *sql.DB
	)
	connFor := func(role string) *sql.DB {
		return map[string]*sql.DB{"app_user": appConn, "graph_user": graphConn}[role]
	}
	totalEvents := func() int {
		var n int
		Expect(suPool.QueryRow(ctx, "SELECT count(*) FROM identity_events").Scan(&n)).To(Succeed())
		return n
	}
	// stored reads tenantID's events as the superuser: the reference the
	// export is checked against. Times are normalized to UTC on both sides.
	stored := func() []db.IdentityEvent {
		rows, err := suPool.Query(ctx, `SELECT event_id, occurred_at, action, kind, tenant_id, identity_id, actor,
			db_user, reason, request, attestation, attestation_sha256
			FROM identity_events WHERE tenant_id = $1 ORDER BY event_id`, tenantID)
		Expect(err).NotTo(HaveOccurred())
		defer rows.Close()
		var events []db.IdentityEvent
		for rows.Next() {
			var e db.IdentityEvent
			Expect(rows.Scan(&e.EventID, &e.OccurredAt, &e.Action, &e.Kind, &e.TenantID, &e.IdentityID, &e.Actor,
				&e.DBUser, &e.Reason, &e.Request, &e.Attestation, &e.AttestationSHA256)).To(Succeed())
			e.OccurredAt = e.OccurredAt.UTC()
			events = append(events, e)
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		return events
	}
	// collect returns an export callback that asserts event_id strictly
	// increases across the whole log and keeps tenantID's events; other
	// specs' events share the log.
	collect := func(into *[]db.IdentityEvent) func(db.IdentityEvent) error {
		var last int64
		return func(e db.IdentityEvent) error {
			Expect(e.EventID).To(BeNumerically(">", last))
			last = e.EventID
			if e.TenantID == tenantID {
				e.OccurredAt = e.OccurredAt.UTC()
				*into = append(*into, e)
			}
			return nil
		}
	}

	BeforeAll(func() {
		if suPool == nil {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}
		kp, err := attestation.NewEphemeralKeyProvider()
		Expect(err).NotTo(HaveOccurred())
		gen, err = attestation.NewGenerator(kp)
		Expect(err).NotTo(HaveOccurred())
		admin, err = db.OpenTenantAdmin(tenantAdminDSN())
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(admin.Close)
		adminConn, appConn, graphConn = tenantAdminConn(), appUserConn(), graphUserConn()
		setupTenant(tenantID, "Identity Export")

		_, err = admin.AddReviewer(ctx, tenantID, rita, actor, reason,
			signedRequest(gen, "add", db.IdentityKindReviewer, tenantID, rita, actor, reason))
		Expect(err).NotTo(HaveOccurred())
		Expect(admin.DeleteTenant(ctx, tenantID, actor, reason,
			signedRequest(gen, "delete", db.IdentityKindTenant, tenantID, tenantID, actor, reason))).To(Succeed())
		Expect(admin.RestoreTenant(ctx, tenantID, actor, reason,
			signedRequest(gen, "restore", db.IdentityKindTenant, tenantID, tenantID, actor, reason))).To(Succeed())
	})

	AfterAll(func() {
		if suPool != nil {
			stmt, err := dbtest.HardDeleteTenantSQL(tenantID)
			Expect(err).NotTo(HaveOccurred())
			Expect(suPool.Exec(ctx, stmt)).To(Succeed())
		}
	})

	It("returns every event in event_id order with the stored bytes intact", func() {
		var got []db.IdentityEvent
		Expect(admin.ExportIdentityEvents(ctx, collect(&got))).To(Succeed())
		Expect(got).To(Equal(stored()))
		Expect(got).To(HaveLen(3))
		var actions []string
		for _, e := range got {
			actions = append(actions, e.Kind+" "+e.Action)
			Expect(e.Actor).To(Equal(actor))
			Expect(e.Reason).To(Equal(reason))
			Expect(e.DBUser).To(Equal("tenant_admin"))
			Expect(e.OccurredAt).NotTo(BeZero())
			attSum := sha256.Sum256(e.Attestation)
			Expect(e.AttestationSHA256).To(Equal(hex.EncodeToString(attSum[:])))
			link, err := gen.Verify(ctx, e.Attestation)
			Expect(err).NotTo(HaveOccurred())
			reqSum := sha256.Sum256(e.Request)
			Expect(link.Materials).To(Equal([]attestation.Artifact{{URI: "crosscodex:identity-request", Digest: hex.EncodeToString(reqSum[:])}}))
		}
		Expect(actions).To(Equal([]string{"reviewer add", "tenant delete", "tenant restore"}))
	})

	DescribeTable("pages through the whole log across page boundaries",
		func(pageSize int) {
			var got []db.IdentityEvent
			Expect(db.ExportIdentityEventsWithPageSize(ctx, admin, pageSize, collect(&got))).To(Succeed())
			Expect(got).To(Equal(stored()))
		},
		Entry("one event per page", 1),
		Entry("two events per page", 2),
		Entry("the largest page", db.MaxIdentityExportPage),
	)

	It("stops at the first callback error and returns it", func() {
		stop := errors.New("disk full")
		calls := 0
		err := admin.ExportIdentityEvents(ctx, func(db.IdentityEvent) error { calls++; return stop })
		Expect(err).To(MatchError(stop))
		Expect(err.Error()).To(HavePrefix("export identity events: "))
		Expect(calls).To(Equal(1))
	})

	DescribeTable("refuses a page size outside 1..10000, reading nothing",
		func(pageSize int) {
			before := totalEvents()
			calls := 0
			err := db.ExportIdentityEventsWithPageSize(ctx, admin, pageSize, func(db.IdentityEvent) error { calls++; return nil })
			Expect(err).To(HaveOccurred())
			Expect(db.IsPgErrorCode(err, "22023")).To(BeTrue(), "got %v", err)
			Expect(err.Error()).To(ContainSubstring("Pass a page size from 1 to 10000"))
			Expect(calls).To(BeZero())
			Expect(totalEvents()).To(Equal(before))
		},
		Entry("zero", 0),
		Entry("one past the largest", db.MaxIdentityExportPage+1),
		Entry("negative", -1),
	)

	It("refuses a NULL starting event_id with the fix", func() {
		_, err := adminConn.ExecContext(ctx, "SELECT * FROM public.export_identity_events(NULL, 10)")
		Expect(err).To(HaveOccurred())
		Expect(db.IsPgErrorCode(err, "22023")).To(BeTrue(), "got %v", err)
		Expect(err.Error()).To(ContainSubstring("Pass 0 to start at the first event"))
	})

	DescribeTable("refuses every application role but tenant_admin, reading nothing",
		func(role string) {
			before := totalEvents()
			_, err := connFor(role).ExecContext(ctx, "SELECT * FROM public.export_identity_events(0, 10)")
			Expect(err).To(HaveOccurred())
			Expect(db.IsPgErrorCode(err, "42501")).To(BeTrue(), "got %v", err)
			Expect(err.Error()).To(ContainSubstring("permission denied for function export_identity_events"))
			Expect(totalEvents()).To(Equal(before))
		},
		Entry(nil, "app_user"),
		Entry(nil, "graph_user"),
	)

	It("is not executable by PUBLIC", func() {
		var ok bool
		Expect(suPool.QueryRow(ctx, "SELECT has_function_privilege('public', 'public.export_identity_events(bigint, integer)', 'EXECUTE')").
			Scan(&ok)).To(Succeed())
		Expect(ok).To(BeFalse())
	})

	It("is a stable definer function with a pinned search_path", func() {
		var definer bool
		var volatility, config string
		Expect(suPool.QueryRow(ctx, `SELECT prosecdef, provolatile::text, coalesce(array_to_string(proconfig, ';'), '')
			FROM pg_proc WHERE oid = 'public.export_identity_events(bigint, integer)'::regprocedure`).
			Scan(&definer, &volatility, &config)).To(Succeed())
		Expect(definer).To(BeTrue())
		Expect(volatility).To(Equal("s"))
		Expect(config).To(Equal("search_path=pg_catalog, public, pg_temp"))
	})
})
