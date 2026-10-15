//go:build integration

package db_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/db/migrations"
)

// Migration 009 (#195) makes judgements permanent: artifact_pair_verdicts
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

// identityRequest is the canonical request an operator signs. Field order is
// fixed by the struct, so the bytes are deterministic.
type identityRequest struct {
	Action      string `json:"action"`
	Kind        string `json:"kind"`
	TenantID    string `json:"tenant_id"`
	IdentityID  string `json:"identity_id"`
	Actor       string `json:"actor"`
	Reason      string `json:"reason"`
	RequestedAt string `json:"requested_at"`
}

// signed returns canonical request bytes and a link envelope whose single
// material digest is sha256(request).
func signed(gen attestation.Generator, action, kind, tenantID, identity, actor, reason string) (req, env []byte) {
	req, err := json.Marshal(identityRequest{
		Action: action, Kind: kind, TenantID: tenantID, IdentityID: identity,
		Actor: actor, Reason: reason, RequestedAt: "2026-10-14T00:00:00Z",
	})
	Expect(err).NotTo(HaveOccurred())
	sum := sha256.Sum256(req)
	link, err := gen.CreateLink(context.Background(), "identity."+action,
		[]attestation.Artifact{{URI: "crosscodex:identity-request", Digest: hex.EncodeToString(sum[:])}}, nil)
	Expect(err).NotTo(HaveOccurred())
	return req, link.Raw
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

// runMigrationFileRolledBack runs setup and then a 009 migration file as
// the superuser in one transaction that is always rolled back, so a
// refusal can be proven without touching the shared database.
func runMigrationFileRolledBack(file string, setup ...string) error {
	ctx := context.Background()
	sqlText, err := fs.ReadFile(migrations.FS, file)
	Expect(err).NotTo(HaveOccurred())
	tx, err := suPool.Begin(ctx)
	Expect(err).NotTo(HaveOccurred())
	defer tx.Rollback() //nolint:errcheck // probe transaction, always rolled back
	Expect(tx.Exec(ctx, "SET LOCAL lock_timeout = '5s'")).To(Succeed())
	for _, stmt := range setup {
		Expect(tx.Exec(ctx, stmt)).To(Succeed(), stmt)
	}
	return tx.Exec(ctx, string(sqlText))
}

// Migration 009 (#195) tenant lifecycle: tenant_admin soft-deletes, restores,
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
	)

	It("declares every definer function with a pinned search_path", func() {
		rows, err := suPool.Query(ctx, `
			SELECT proname, prosecdef, coalesce(array_to_string(proconfig, ';'), '')
			FROM pg_proc
			WHERE pronamespace = 'public'::regnamespace
			  AND proname IN ('delete_tenant', 'restore_tenant', 'hide_tenant', 'unhide_tenant',
			                  'list_tenants', 'set_tenant_status', 'provision_tenant', 'enter_tenant')
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
		Expect(names).To(HaveLen(8))
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

	It("refuses to apply 009 over a tenant that already uses the 'hidden-' prefix", func() {
		expectRefusedUnchanged(liveID, func() error {
			return runMigrationFileRolledBack("009_identity_lifecycle.up.sql",
				"SET LOCAL session_replication_role = replica", // skip the graph-creating trigger
				"ALTER TABLE tenants DROP CONSTRAINT tenants_id_not_reserved",
				"INSERT INTO tenants (tenant_id, display_name) VALUES ('hidden-legacy', 'Legacy')")
		}, "23514", "'hidden-legacy'")
		Expect(tenantSnapshot("hidden-legacy")).To(BeEmpty())
	})

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

	It("refuses to roll back 009 while a tenant is deleted", func() {
		expectRefusedUnchanged(deletedID, func() error {
			return runMigrationFileRolledBack("009_identity_lifecycle.down.sql")
		}, "P0001", "also refused while identity_events holds audit records")
		Expect(tenantField(deletedID, "status").String).To(Equal("deleted"))
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

	It("refuses to roll back 009 while identity_events holds audit records", func() {
		expectRefusedUnchanged(deletedID, func() error {
			return runMigrationFileRolledBack("009_identity_lifecycle.down.sql")
		}, "P0001", "audit records, which are never deleted")
		Expect(eventCount(deletedID)).To(Equal(6))
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

// humanVerdictSQL inserts a human verdict by determinedBy into tenantID,
// keyed by pairKey. It runs as the superuser, bypassing the reviewer gate.
func humanVerdictSQL(tenantID, pairKey, determinedBy string) string {
	return fmt.Sprintf(`INSERT INTO artifact_pair_verdicts
		(tenant_id, pair_key, low_group_id, high_group_id, candidate_edge_id, similarity_score, context,
		 status, determination_type, determined_by, next_attempt_at, created_at, decided_at)
		VALUES (%s, %s, 'a', 'b', 'e', 0.5, '{}', 'confirmed', 'human', %s, now(), now(), now())`,
		quoteLiteral(tenantID), quoteLiteral(pairKey), quoteLiteral(determinedBy))
}

// quoteLiteral quotes s as a SQL string literal for test-built statements.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// Migration 009 (#195) reviewer registry: tenant_admin adds, soft-deletes,
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

	It("backfills every reviewer of an existing human verdict, re-runnably", func() {
		Expect(suPool.Exec(ctx, humanVerdictSQL(tenantID, "backfill-1", "legacy@example.invalid"))).To(Succeed())
		Expect(suPool.Exec(ctx, humanVerdictSQL(tenantID, "backfill-2", "legacy@example.invalid"))).To(Succeed())
		Expect(suPool.Exec(ctx, `INSERT INTO artifact_pair_verdicts
			(tenant_id, pair_key, low_group_id, high_group_id, candidate_edge_id, similarity_score, context,
			 determination_type, determined_by, next_attempt_at, created_at)
			VALUES ($1, 'backfill-3', 'a', 'b', 'e', 0.5, '{}', 'llm_panel', 'panel-model', now(), now())`, tenantID)).To(Succeed())
		// The backfill statement is read from the migration itself and run
		// twice to prove it is re-runnable.
		upSQL, err := fs.ReadFile(migrations.FS, "009_identity_lifecycle.up.sql")
		Expect(err).NotTo(HaveOccurred())
		found := regexp.MustCompile(`(?s)INSERT INTO public\.reviewers \(tenant_id, reviewer_id, created_by\)\s+SELECT DISTINCT .*?ON CONFLICT DO NOTHING;`).
			FindAllString(string(upSQL), -1)
		Expect(found).To(HaveLen(1), "009 must contain exactly one reviewers backfill statement")
		backfill := found[0]
		Expect(suPool.Exec(ctx, backfill)).To(Succeed())
		row := reviewerSnapshot(tenantID, "legacy@example.invalid")
		Expect(suPool.Exec(ctx, backfill)).To(Succeed())
		Expect(reviewerSnapshot(tenantID, "legacy@example.invalid")).To(Equal(row))
		Expect(reviewerField(tenantID, "legacy@example.invalid", "created_by").String).To(Equal("migration-009"))
		Expect(reviewerField(tenantID, "legacy@example.invalid", "status").String).To(Equal("active"))
		Expect(reviewerSnapshot(tenantID, "panel-model")).To(BeEmpty())
	})

	DescribeTable("refuses to apply 009 over a human verdict whose reviewer cannot be registered",
		func(determinedBy, wantMsg string) {
			expectRefusedUnchanged(tenantID, determinedBy, func() error {
				return runMigrationFileRolledBack("009_identity_lifecycle.up.sql",
					humanVerdictSQL(tenantID, "legacy-bad", determinedBy))
			}, "23514", wantMsg)
			Expect(reviewerSnapshot(tenantID, determinedBy)).To(BeEmpty())
			var n int
			Expect(suPool.QueryRow(ctx, "SELECT count(*) FROM artifact_pair_verdicts WHERE pair_key = 'legacy-bad'").Scan(&n)).To(Succeed())
			Expect(n).To(BeZero())
		},
		Entry("a pseudonym-shaped reviewer ID", reservedID, "'"+reservedID+"'"),
		Entry("a reviewer ID with a leading space", " lead", "' lead'"),
		Entry("a reviewer ID with a control character, escaped", "ctl\x01x", `'ctl\u0001x'`),
	)

	It("lists at most 20 unregistrable reviewer IDs when refusing to apply 009", func() {
		var setup []string
		for i := range 25 {
			setup = append(setup, humanVerdictSQL(tenantID, fmt.Sprintf("legacy-bad-%02d", i), fmt.Sprintf("hidden-legacy-%02d", i)))
		}
		events := eventCount(tenantID)
		err := runMigrationFileRolledBack("009_identity_lifecycle.up.sql", setup...)
		Expect(err).To(HaveOccurred())
		Expect(db.IsPgErrorCode(err, "23514")).To(BeTrue(), "got %v", err)
		Expect(err.Error()).To(ContainSubstring("'hidden-legacy-19'"))
		Expect(err.Error()).NotTo(ContainSubstring("'hidden-legacy-20'"))
		Expect(err.Error()).To(ContainSubstring("and 5 more"))
		Expect(eventCount(tenantID)).To(Equal(events))
		var n int
		Expect(suPool.QueryRow(ctx, "SELECT count(*) FROM artifact_pair_verdicts WHERE pair_key LIKE 'legacy-bad-%'").Scan(&n)).To(Succeed())
		Expect(n).To(BeZero())
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
			reviewerPseudonym:        "deleted hidden=true",
			bob:                      "active hidden=false",
			"legacy@example.invalid": "active hidden=false",
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
		Expect(total).To(Equal(3)) // alice, bob, legacy
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
