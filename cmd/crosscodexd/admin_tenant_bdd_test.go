//go:build !integration

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
)

const testTenantAdminDSN = "postgres://tenant_admin@db:5432/crosscodex"

var _ = Describe("runAdmin tenant arg parsing", func() {
	DescribeTable("returns 2 for usage errors",
		func(args ...string) {
			Expect(runAdmin(args)).To(Equal(2))
		},
		Entry("tenant without a command", "tenant"),
		Entry("unknown tenant command", "tenant", "bogus"),
		Entry("create without --tenant", "tenant", "create", "--display-name", "Acme"),
		Entry("create without --display-name", "tenant", "create", "--tenant", "acme"),
		Entry("create with a blank --display-name", "tenant", "create", "--tenant", "acme", "--display-name", "   "),
		Entry("create with a malformed --tenant", "tenant", "create", "--tenant", "Acme_Corp", "--display-name", "Acme"),
		Entry("create with an extra argument", "tenant", "create", "--tenant", "acme", "--display-name", "Acme", "x"),
		Entry("list with an argument", "tenant", "list", "x"),
		Entry("list with an unknown flag", "tenant", "list", "--all"),
		Entry("inspect without --tenant", "tenant", "inspect"),
		Entry("suspend without --tenant", "tenant", "suspend"),
		Entry("resume with a malformed --tenant", "tenant", "resume", "--tenant", "-acme"),
		Entry("import without --file", "tenant", "import"),
	)

	DescribeTable("returns 0 for -h",
		func(args ...string) {
			Expect(runAdmin(args)).To(Equal(0))
		},
		Entry("create -h", "tenant", "create", "-h"),
		Entry("list -h", "tenant", "list", "-h"),
		Entry("inspect -h", "tenant", "inspect", "-h"),
		Entry("suspend -h", "tenant", "suspend", "-h"),
		Entry("resume -h", "tenant", "resume", "-h"),
		Entry("import -h", "tenant", "import", "-h"),
	)
})

var _ = Describe("runAdmin tenant commands", func() {
	var (
		origCreate    func(context.Context, string, string, string) (bool, error)
		origList      func(context.Context, string) ([]dbpkg.TenantRecord, error)
		origInspect   func(context.Context, string, string) (dbpkg.TenantRecord, error)
		origSetStatus func(context.Context, string, string, string) (string, error)
		origImport    func(context.Context, string, []dbpkg.TenantSpec) ([]bool, error)

		fnErr     error
		seamCalls int
		gotDSN    string
		gotArgs   []string
		created   bool
		previous  string
		gotSpecs  []dbpkg.TenantSpec
	)
	record := dbpkg.TenantRecord{ID: "acme", DisplayName: "Acme Corp", Status: "active", CreatedAt: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}

	BeforeEach(func() {
		origCreate, origList, origInspect, origSetStatus, origImport = runTenantCreateFn, runTenantListFn, runTenantInspectFn, runTenantSetStatusFn, runTenantImportFn
		fnErr, seamCalls, gotDSN, gotArgs, created, previous, gotSpecs = nil, 0, "", nil, true, "active", nil
		runTenantCreateFn = func(_ context.Context, dsn, id, name string) (bool, error) {
			seamCalls++
			gotDSN, gotArgs = dsn, []string{id, name}
			return created, fnErr
		}
		runTenantListFn = func(_ context.Context, dsn string) ([]dbpkg.TenantRecord, error) {
			seamCalls++
			gotDSN = dsn
			return []dbpkg.TenantRecord{record}, fnErr
		}
		runTenantInspectFn = func(_ context.Context, dsn, id string) (dbpkg.TenantRecord, error) {
			seamCalls++
			gotDSN, gotArgs = dsn, []string{id}
			return record, fnErr
		}
		runTenantSetStatusFn = func(_ context.Context, dsn, id, status string) (string, error) {
			seamCalls++
			gotDSN, gotArgs = dsn, []string{id, status}
			return previous, fnErr
		}
		runTenantImportFn = func(_ context.Context, dsn string, specs []dbpkg.TenantSpec) ([]bool, error) {
			seamCalls++
			gotDSN, gotSpecs = dsn, specs
			out := make([]bool, len(specs))
			out[0] = true
			return out, fnErr
		}
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		GinkgoT().Setenv("CROSSCODEX_DATABASE_TENANT_ADMIN_DSN", testTenantAdminDSN)
	})

	AfterEach(func() {
		runTenantCreateFn, runTenantListFn, runTenantInspectFn, runTenantSetStatusFn, runTenantImportFn = origCreate, origList, origInspect, origSetStatus, origImport
	})

	writeImportFile := func(content string) string {
		path := filepath.Join(GinkgoT().TempDir(), "tenants.yaml")
		Expect(os.WriteFile(path, []byte(content), 0o600)).To(Succeed())
		return path
	}

	DescribeTable("refuses with the fix when database.tenant_admin_dsn is unset",
		func(args ...string) {
			GinkgoT().Setenv("CROSSCODEX_DATABASE_TENANT_ADMIN_DSN", "")
			code, stderr := captureStderr(func() int { return runAdmin(args) })
			Expect(code).To(Equal(1))
			Expect(stderr).To(ContainSubstring("database.tenant_admin_dsn is not set. Set CROSSCODEX_DATABASE_TENANT_ADMIN_DSN to a DSN for the tenant_admin role (created by migration 007_tenant_admin)."))
			Expect(seamCalls).To(BeZero())
		},
		Entry("create", "tenant", "create", "--tenant", "acme", "--display-name", "Acme"),
		Entry("list", "tenant", "list"),
		Entry("inspect", "tenant", "inspect", "--tenant", "acme"),
		Entry("suspend", "tenant", "suspend", "--tenant", "acme"),
		Entry("resume", "tenant", "resume", "--tenant", "acme"),
	)

	It("import refuses with the fix when database.tenant_admin_dsn is unset", func() {
		path := writeImportFile("tenants:\n  - tenant_id: acme\n    display_name: Acme Corp\n")
		GinkgoT().Setenv("CROSSCODEX_DATABASE_TENANT_ADMIN_DSN", "")
		code, stderr := captureStderr(func() int { return runAdmin([]string{"tenant", "import", "--file", path}) })
		Expect(code).To(Equal(1))
		Expect(stderr).To(ContainSubstring("database.tenant_admin_dsn is not set. Set CROSSCODEX_DATABASE_TENANT_ADMIN_DSN to a DSN for the tenant_admin role (created by migration 007_tenant_admin)."))
		Expect(seamCalls).To(BeZero())
	})

	DescribeTable("exits 1 and prefixes the command on a seam failure",
		func(prefix string, args ...string) {
			fnErr = errors.New("boom")
			var code int
			var stderr string
			_, stdout := captureStream(&os.Stdout, func() int {
				code, stderr = captureStderr(func() int { return runAdmin(args) })
				return code
			})
			Expect(code).To(Equal(1))
			Expect(stderr).To(ContainSubstring(prefix + ": boom"))
			Expect(stdout).To(BeEmpty())
		},
		Entry("create", "admin tenant create", "tenant", "create", "--tenant", "acme", "--display-name", "Acme"),
		Entry("list", "admin tenant list", "tenant", "list"),
		Entry("inspect", "admin tenant inspect", "tenant", "inspect", "--tenant", "acme"),
		Entry("suspend", "admin tenant suspend", "tenant", "suspend", "--tenant", "acme"),
		Entry("resume", "admin tenant resume", "tenant", "resume", "--tenant", "acme"),
	)

	It("import exits 1 and prefixes the command on a seam failure", func() {
		path := writeImportFile("tenants:\n  - tenant_id: acme\n    display_name: Acme Corp\n")
		fnErr = errors.New("boom")
		var code int
		var stderr string
		_, stdout := captureStream(&os.Stdout, func() int {
			code, stderr = captureStderr(func() int { return runAdmin([]string{"tenant", "import", "--file", path}) })
			return code
		})
		Expect(code).To(Equal(1))
		Expect(stderr).To(ContainSubstring("admin tenant import: boom"))
		Expect(stdout).To(BeEmpty())
	})

	It("create passes the DSN and flags through and reports created or updated", func() {
		code, stdout := captureStream(&os.Stdout, func() int {
			return runAdmin([]string{"tenant", "create", "--tenant", "acme", "--display-name", "Acme Corp"})
		})
		Expect(code).To(Equal(0))
		Expect(gotDSN).To(Equal(testTenantAdminDSN))
		Expect(gotArgs).To(Equal([]string{"acme", "Acme Corp"}))
		Expect(stdout).To(Equal("created\n"))

		created = false
		_, stdout = captureStream(&os.Stdout, func() int {
			return runAdmin([]string{"tenant", "create", "--tenant", "acme", "--display-name", "Acme Corp"})
		})
		Expect(stdout).To(Equal("updated\n"))
	})

	DescribeTable("suspend and resume report the transition",
		func(sub, prev, wantStatus, wantOut string) {
			previous = prev
			code, stdout := captureStream(&os.Stdout, func() int {
				return runAdmin([]string{"tenant", sub, "--tenant", "acme"})
			})
			Expect(code).To(Equal(0))
			Expect(gotArgs).To(Equal([]string{"acme", wantStatus}))
			Expect(stdout).To(Equal(wantOut + "\n"))
		},
		Entry("suspend an active tenant", "suspend", "active", "suspended", "suspended"),
		Entry("suspend a suspended tenant", "suspend", "suspended", "suspended", "already suspended"),
		Entry("resume a suspended tenant", "resume", "suspended", "active", "resumed"),
		Entry("resume an active tenant", "resume", "active", "active", "already active"),
	)

	It("list prints one row per tenant", func() {
		code, stdout := captureStream(&os.Stdout, func() int { return runAdmin([]string{"tenant", "list"}) })
		Expect(code).To(Equal(0))
		lines := strings.Split(strings.TrimSpace(stdout), "\n")
		Expect(lines).To(HaveLen(2))
		Expect(strings.Fields(lines[0])).To(Equal([]string{"TENANT", "DISPLAY", "NAME", "STATUS", "CREATED"}))
		Expect(lines[1]).To(ContainSubstring("acme"))
		Expect(lines[1]).To(ContainSubstring("Acme Corp"))
		Expect(lines[1]).To(ContainSubstring("2026-10-08T12:00:00Z"))
	})

	It("list quotes control characters in stored values", func() {
		record.DisplayName = "Evil\x1b[2J\tX"
		DeferCleanup(func() { record.DisplayName = "Acme Corp" })
		code, stdout := captureStream(&os.Stdout, func() int { return runAdmin([]string{"tenant", "list"}) })
		Expect(code).To(Equal(0))
		Expect(stdout).NotTo(ContainSubstring("\x1b"))
		Expect(stdout).NotTo(ContainSubstring("Evil\x1b[2J\tX"))
		Expect(stdout).To(ContainSubstring(`"Evil\x1b[2J\tX"`))
	})

	It("inspect prints FIELD VALUE rows", func() {
		code, stdout := captureStream(&os.Stdout, func() int { return runAdmin([]string{"tenant", "inspect", "--tenant", "acme"}) })
		Expect(code).To(Equal(0))
		Expect(gotArgs).To(Equal([]string{"acme"}))
		Expect(stdout).To(MatchRegexp(`(?m)^FIELD\s+VALUE$`))
		Expect(stdout).To(MatchRegexp(`(?m)^display_name\s+Acme Corp$`))
		Expect(stdout).To(MatchRegexp(`(?m)^status\s+active$`))
		Expect(stdout).To(MatchRegexp(`(?m)^created_at\s+2026-10-08T12:00:00Z$`))
	})

	It("inspect quotes control characters in stored values", func() {
		record.DisplayName = "Evil\x1b[2J"
		DeferCleanup(func() { record.DisplayName = "Acme Corp" })
		code, stdout := captureStream(&os.Stdout, func() int { return runAdmin([]string{"tenant", "inspect", "--tenant", "acme"}) })
		Expect(code).To(Equal(0))
		Expect(stdout).NotTo(ContainSubstring("\x1b"))
		Expect(stdout).To(ContainSubstring(`"Evil\x1b[2J"`))
	})

	It("import parses the file and prints one result per tenant", func() {
		path := writeImportFile("tenants:\n  - tenant_id: acme\n    display_name: Acme Corp\n  - tenant_id: globex\n    display_name: Globex\n")
		code, stdout := captureStream(&os.Stdout, func() int { return runAdmin([]string{"tenant", "import", "--file", path}) })
		Expect(code).To(Equal(0))
		Expect(gotSpecs).To(Equal([]dbpkg.TenantSpec{{ID: "acme", DisplayName: "Acme Corp"}, {ID: "globex", DisplayName: "Globex"}}))
		Expect(stdout).To(MatchRegexp(`(?m)^TENANT\s+RESULT$`))
		Expect(stdout).To(MatchRegexp(`(?m)^acme\s+created$`))
		Expect(stdout).To(MatchRegexp(`(?m)^globex\s+updated$`))
	})

	DescribeTable("import rejects a bad file before connecting",
		func(content, want string) {
			path := writeImportFile(content)
			code, stderr := captureStderr(func() int { return runAdmin([]string{"tenant", "import", "--file", path}) })
			Expect(code).To(Equal(1))
			Expect(stderr).To(ContainSubstring("admin tenant import: " + path))
			Expect(stderr).To(ContainSubstring(want))
			Expect(seamCalls).To(BeZero())
		},
		Entry("empty file", "", "file is empty"),
		Entry("empty list", "tenants: []\n", "the list is empty"),
		Entry("unknown field", "tenants:\n  - tenant_id: acme\n    display_name: A\n    status: suspended\n", "field status not found"),
		Entry("malformed ID", "tenants:\n  - tenant_id: Acme_Corp\n    display_name: A\n", "Choose an ID that matches"),
		Entry("empty display name", "tenants:\n  - tenant_id: acme\n    display_name: \"\"\n", "display name is empty"),
		Entry("duplicate IDs", "tenants:\n  - tenant_id: acme\n    display_name: A\n  - tenant_id: globex\n    display_name: G\n  - tenant_id: acme\n    display_name: B\n",
			`tenants 1 and 3 both have ID "acme"`),
		Entry("two documents", "tenants:\n  - tenant_id: acme\n    display_name: A\n---\ntenants:\n  - tenant_id: globex\n    display_name: G\n", "more than one YAML document"),
	)

	It("import exits 1 when the file cannot be read", func() {
		missing := filepath.Join(GinkgoT().TempDir(), "missing.yaml")
		code, stderr := captureStderr(func() int { return runAdmin([]string{"tenant", "import", "--file", missing}) })
		Expect(code).To(Equal(1))
		Expect(stderr).To(ContainSubstring("admin tenant import: "))
		Expect(seamCalls).To(BeZero())
	})
})

var _ = Describe("tenant admin wiring", func() {
	It("reports an unreachable database without leaking the DSN password", func() {
		dsn := "postgres://tenant_admin:s3cret-pw@127.0.0.1:1/crosscodex?sslmode=disable&connect_timeout=1"
		_, err := daemonTenantList(context.Background(), dsn)
		Expect(err).To(MatchError(ContainSubstring("connect as tenant_admin")))
		Expect(err.Error()).NotTo(ContainSubstring("s3cret-pw"))
	})
})

var _ = Describe("tenant output", func() {
	It("list says so when there are no tenants", func() {
		var buf bytes.Buffer
		writeTenantList(&buf, nil)
		Expect(buf.String()).To(Equal("no tenants\n"))
	})

	It("import quotes control characters in tenant IDs", func() {
		var buf bytes.Buffer
		writeTenantImport(&buf, []dbpkg.TenantSpec{{ID: "evil\x1b[2J", DisplayName: "Evil"}}, []bool{true})
		Expect(buf.String()).NotTo(ContainSubstring("\x1b"))
		Expect(buf.String()).To(ContainSubstring(`"evil\x1b[2J"`))
	})
})
