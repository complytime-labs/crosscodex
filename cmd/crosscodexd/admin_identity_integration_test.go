//go:build integration

package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
)

// The identity commands run end to end here: config loading, signing with
// the suite's attestation keys, and the real TenantAdmin calls as
// tenant_admin. Ginkgo runs one package's suite at a time, so setting
// tenant_admin's password here cannot race pkg/db's suite, which sets its own.
var _ = Describe("admin identity commands against a real database", Ordered, func() {
	const (
		tenantID = "cli-identity"
		reviewer = "alice-cli"
		actor    = "ops-cli"
		reason   = "integration lifecycle"
	)
	var (
		ctx  = context.Background()
		suDB *sql.DB
	)

	run := func(args ...string) (int, string, string) {
		var stdout string
		code, stderr := captureStderr(func() int {
			var code int
			code, stdout = captureStream(&os.Stdout, func() int { return runAdmin(args) })
			return code
		})
		return code, stdout, stderr
	}
	audit := func(args ...string) []string { return append(args, "--by", actor, "--reason", reason) }
	hardDelete := func() {
		stmt, err := dbtest.HardDeleteTenantSQL(tenantID)
		Expect(err).NotTo(HaveOccurred())
		_, err = suDB.ExecContext(ctx, stmt)
		Expect(err).NotTo(HaveOccurred(), "hard-delete test tenant")
	}
	eventCount := func() int {
		var n int
		Expect(suDB.QueryRowContext(ctx, "SELECT count(*) FROM identity_events WHERE tenant_id = $1", tenantID).Scan(&n)).To(Succeed())
		return n
	}

	BeforeAll(func() {
		dsn := os.Getenv("TEST_DATABASE_DSN")
		if dsn == "" {
			Skip("TEST_DATABASE_DSN not set — run: task test:integration:db")
		}
		migrator, err := db.NewMigrator(dsn)
		Expect(err).NotTo(HaveOccurred())
		if err := migrator.Up(ctx); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			Expect(err).NotTo(HaveOccurred(), "run migrations")
		}
		Expect(migrator.Close()).To(Succeed())

		suDB, err = sql.Open("pgx", dsn)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(suDB.Close()).To(Succeed()) })
		hardDelete()
		DeferCleanup(hardDelete)

		password, err := dbtest.NewRolePassword()
		Expect(err).NotTo(HaveOccurred())
		stmt, err := dbtest.AlterRolePasswordSQL("tenant_admin", password)
		Expect(err).NotTo(HaveOccurred())
		_, err = suDB.ExecContext(ctx, stmt)
		Expect(err).NotTo(HaveOccurred(), "set tenant_admin password")
		u, err := url.Parse(dsn)
		Expect(err).NotTo(HaveOccurred())
		u.User = url.UserPassword("tenant_admin", password)

		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		GinkgoT().Setenv("CROSSCODEX_DATABASE_TENANT_ADMIN_DSN", u.String())
		GinkgoT().Setenv("CROSSCODEX_ATTESTATION_PRIVATE_KEY_PATH", testAttestationPrivateKeyPath)
		GinkgoT().Setenv("CROSSCODEX_ATTESTATION_PUBLIC_KEY_PATH", testAttestationPublicKeyPath)
	})

	It("runs a reviewer and tenant lifecycle, hiding and tracing the reviewer", func() {
		By("provisioning the tenant")
		code, stdout, stderr := run("tenant", "create", "--tenant", tenantID, "--display-name", "CLI Identity")
		Expect(code).To(Equal(0), stderr)
		Expect(stdout).To(Equal("created\n"))

		By("adding, deleting and hiding a reviewer")
		for _, step := range []struct{ verb, out string }{{"add", "added"}, {"delete", "deleted"}} {
			code, stdout, stderr = run(audit("reviewer", step.verb, "--tenant", tenantID, "--reviewer", reviewer)...)
			Expect(code).To(Equal(0), stderr)
			Expect(stdout).To(Equal(step.out + "\n"))
		}
		code, stdout, stderr = run(audit("reviewer", "hide", "--tenant", tenantID, "--reviewer", reviewer)...)
		Expect(code).To(Equal(0), stderr)
		m := regexp.MustCompile(`^hidden as (hidden-reviewer-[0-9a-f]{16})\n$`).FindStringSubmatch(stdout)
		Expect(m).To(HaveLen(2), stdout)
		pseudonym := m[1]

		By("listing the hidden reviewer only by pseudonym")
		code, stdout, stderr = run("reviewer", "list", "--tenant", tenantID)
		Expect(code).To(Equal(0), stderr)
		Expect(stdout).To(MatchRegexp(`(?m)^` + pseudonym + ` +deleted +\S+ +` + actor + ` +\S+ +yes$`))
		Expect(stdout).NotTo(ContainSubstring(reviewer))

		By("tracing the pseudonym back to the reviewer")
		code, stdout, stderr = run(audit("identity", "trace", "--pseudonym", pseudonym)...)
		Expect(code).To(Equal(0), stderr)
		for _, line := range []string{`^kind +reviewer$`, `^tenant +` + tenantID + `$`, `^identity +` + reviewer + `$`,
			`^status +deleted$`, `^hidden +yes$`} {
			Expect(stdout).To(MatchRegexp(`(?m)` + line))
		}

		By("deleting and restoring the tenant, which comes back suspended")
		code, stdout, stderr = run(audit("tenant", "delete", "--tenant", tenantID)...)
		Expect(code).To(Equal(0), stderr)
		Expect(stdout).To(Equal("deleted\n"))
		code, stdout, stderr = run(audit("tenant", "restore", "--tenant", tenantID)...)
		Expect(code).To(Equal(0), stderr)
		Expect(stdout).To(ContainSubstring("the tenant is suspended. Run 'crosscodexd admin tenant resume --tenant " + tenantID + "'"))
		code, stdout, stderr = run("tenant", "inspect", "--tenant", tenantID)
		Expect(code).To(Equal(0), stderr)
		Expect(stdout).To(MatchRegexp(`(?m)^status +suspended$`))
		Expect(stdout).To(MatchRegexp(`(?m)^deleted_at +-$`))

		By("recording one signed event per action, in order")
		gen, err := attestation.NewGenerator(&attestation.FileKeyProvider{
			PrivateKeyPath: testAttestationPrivateKeyPath, PublicKeyPath: testAttestationPublicKeyPath})
		Expect(err).NotTo(HaveOccurred())
		rows, err := suDB.QueryContext(ctx, `SELECT action, kind, identity_id, actor, reason, request, attestation, attestation_sha256
			FROM identity_events WHERE tenant_id = $1 ORDER BY event_id`, tenantID)
		Expect(err).NotTo(HaveOccurred())
		defer func() { Expect(rows.Close()).To(Succeed()) }()
		var actions []string
		for rows.Next() {
			var action, kind, identity, gotActor, gotReason, attSHA string
			var request, att []byte
			Expect(rows.Scan(&action, &kind, &identity, &gotActor, &gotReason, &request, &att, &attSHA)).To(Succeed())
			actions = append(actions, kind+" "+action)
			Expect(gotActor).To(Equal(actor))
			Expect(gotReason).To(Equal(reason))

			var body map[string]string
			Expect(json.Unmarshal(request, &body)).To(Succeed())
			Expect(body).To(HaveKeyWithValue("action", action))
			Expect(body).To(HaveKeyWithValue("actor", actor))
			Expect(body).To(HaveKeyWithValue("reason", reason))
			if action == "trace" {
				Expect(body).To(HaveKeyWithValue("identity_id", pseudonym))
				Expect(identity).To(Equal(reviewer))
			} else {
				Expect(body).To(HaveKeyWithValue("kind", kind))
				Expect(body).To(HaveKeyWithValue("tenant_id", tenantID))
				Expect(body).To(HaveKeyWithValue("identity_id", identity))
			}

			link, err := gen.Verify(ctx, att)
			Expect(err).NotTo(HaveOccurred(), "attestation of %s %s verifies", kind, action)
			Expect(link.Step).To(Equal("identity." + action))
			sum := sha256.Sum256(request)
			Expect(link.Materials).To(Equal([]attestation.Artifact{{URI: "crosscodex:identity-request", Digest: hex.EncodeToString(sum[:])}}))
			attSum := sha256.Sum256(att)
			Expect(attSHA).To(Equal(hex.EncodeToString(attSum[:])))
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		Expect(actions).To(Equal([]string{"reviewer add", "reviewer delete", "reviewer hide", "reviewer trace", "tenant delete", "tenant restore"}))
	})

	It("exports the audit log to a 0600 JSONL file with the stored bytes, leaving no partial file", func() {
		dir := GinkgoT().TempDir()
		out := filepath.Join(dir, "audit.jsonl")
		code, stdout, stderr := run("identity", "export", "--out", out)
		Expect(code).To(Equal(0), stderr)
		Expect(stdout).To(MatchRegexp(`^exported [0-9]+ identity events to ` + regexp.QuoteMeta(out) + `\n$`))
		entries, err := os.ReadDir(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(HaveLen(1), "no .partial file is left behind")
		info, err := os.Stat(out)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)))

		// want is this tenant's events as stored, read as the superuser.
		type event struct {
			EventID           int64  `json:"event_id"`
			OccurredAt        string `json:"occurred_at"`
			Action            string `json:"action"`
			Kind              string `json:"kind"`
			TenantID          string `json:"tenant_id"`
			IdentityID        string `json:"identity_id"`
			Actor             string `json:"actor"`
			DBUser            string `json:"db_user"`
			Reason            string `json:"reason"`
			Request           []byte `json:"request"`
			Attestation       []byte `json:"attestation"`
			AttestationSHA256 string `json:"attestation_sha256"`
		}
		rows, err := suDB.QueryContext(ctx, `SELECT event_id, action, kind, identity_id, actor, db_user, reason, request,
			attestation, attestation_sha256 FROM identity_events WHERE tenant_id = $1 ORDER BY event_id`, tenantID)
		Expect(err).NotTo(HaveOccurred())
		defer func() { Expect(rows.Close()).To(Succeed()) }()
		var want []event
		for rows.Next() {
			e := event{TenantID: tenantID}
			Expect(rows.Scan(&e.EventID, &e.Action, &e.Kind, &e.IdentityID, &e.Actor, &e.DBUser, &e.Reason, &e.Request,
				&e.Attestation, &e.AttestationSHA256)).To(Succeed())
			want = append(want, e)
		}
		Expect(rows.Err()).NotTo(HaveOccurred())
		Expect(want).NotTo(BeEmpty())

		body, err := os.ReadFile(out)
		Expect(err).NotTo(HaveOccurred())
		Expect(body).To(HaveSuffix("\n"))
		var got []event
		var last int64
		for _, line := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
			var e event
			Expect(json.Unmarshal([]byte(line), &e)).To(Succeed(), line)
			Expect(e.EventID).To(BeNumerically(">", last), "events are in event_id order")
			last = e.EventID
			Expect(e.OccurredAt).To(MatchRegexp(`Z$`))
			attSum := sha256.Sum256(e.Attestation)
			Expect(e.AttestationSHA256).To(Equal(hex.EncodeToString(attSum[:])))
			if e.TenantID == tenantID {
				e.OccurredAt = ""
				got = append(got, e)
			}
		}
		Expect(got).To(Equal(want))
	})

	It("refuses to overwrite an existing --out file, leaving it unchanged", func() {
		out := filepath.Join(GinkgoT().TempDir(), "audit.jsonl")
		Expect(os.WriteFile(out, []byte("earlier export\n"), 0o600)).To(Succeed())
		code, stdout, stderr := run("identity", "export", "--out", out)
		Expect(code).To(Equal(1))
		Expect(stderr).To(ContainSubstring("already exists; nothing was written. Choose a new --out path"))
		Expect(stdout).To(BeEmpty())
		body, err := os.ReadFile(out)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(body)).To(Equal("earlier export\n"))
	})

	It("refuses to hide a tenant that is not deleted, with the fix, recording nothing", func() {
		before := eventCount()
		code, stdout, stderr := run(audit("tenant", "hide", "--tenant", tenantID)...)
		Expect(code).To(Equal(1))
		Expect(stderr).To(ContainSubstring("only a deleted tenant can be hidden"))
		Expect(stderr).To(ContainSubstring("Delete it first with 'crosscodexd admin tenant delete'"))
		Expect(stdout).To(BeEmpty())
		Expect(eventCount()).To(Equal(before))
		code, stdout, _ = run("tenant", "inspect", "--tenant", tenantID)
		Expect(code).To(Equal(0))
		Expect(stdout).To(MatchRegexp(`(?m)^status +suspended$`))
	})
})
