//go:build !integration

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/attestation"
	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
)

const testPseudonym = "hidden-tenant-0123456789abcdef"

// writeIdentityKeys writes an ECDSA key pair on curve as PEM files, the form
// attestation.FileKeyProvider reads, and returns their paths.
func writeIdentityKeys(curve elliptic.Curve) (privPath, pubPath string) {
	dir := GinkgoT().TempDir()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	privDER, err := x509.MarshalECPrivateKey(key)
	Expect(err).NotTo(HaveOccurred())
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	Expect(err).NotTo(HaveOccurred())
	privPath, pubPath = filepath.Join(dir, "identity-private.pem"), filepath.Join(dir, "identity-public.pem")
	Expect(os.WriteFile(privPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privDER}), 0o600)).To(Succeed())
	Expect(os.WriteFile(pubPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o644)).To(Succeed())
	return privPath, pubPath
}

var _ = Describe("runAdmin identity arg parsing", func() {
	var seamCalls int

	BeforeEach(func() {
		origAction, origList, origTrace := runIdentityActionFn, runReviewerListFn, runTraceFn
		seamCalls = 0
		runIdentityActionFn = func(context.Context, string, dbpkg.IdentityRequest, dbpkg.SignedRequest) (identityResult, error) {
			seamCalls++
			return identityResult{}, nil
		}
		runReviewerListFn = func(context.Context, string, string) ([]dbpkg.ReviewerRecord, error) {
			seamCalls++
			return nil, nil
		}
		runTraceFn = func(context.Context, string, dbpkg.IdentityRequest, dbpkg.SignedRequest) (dbpkg.TraceResult, error) {
			seamCalls++
			return dbpkg.TraceResult{}, nil
		}
		DeferCleanup(func() { runIdentityActionFn, runReviewerListFn, runTraceFn = origAction, origList, origTrace })
	})

	DescribeTable("returns 2 with an actionable message for usage errors, before any call",
		func(want string, args ...string) {
			code, stderr := captureStderr(func() int { return runAdmin(args) })
			Expect(code).To(Equal(2))
			Expect(stderr).To(ContainSubstring(want))
			Expect(seamCalls).To(BeZero())
		},
		Entry("tenant delete without --by", "--by is empty: name the operator making this change",
			"tenant", "delete", "--tenant", "acme", "--reason", "gone"),
		Entry("tenant delete without --reason", "--reason is empty: say why this change is made",
			"tenant", "delete", "--tenant", "acme", "--by", "ops"),
		Entry("tenant delete without --tenant", "admin tenant delete: --tenant is required",
			"tenant", "delete", "--by", "ops", "--reason", "gone"),
		Entry("tenant hide with a malformed --tenant", "admin tenant hide: --tenant:",
			"tenant", "hide", "--tenant", "Acme_Corp", "--by", "ops", "--reason", "gone"),
		Entry("tenant restore with a control character in --reason", "--reason contains control, format or line-separator characters",
			"tenant", "restore", "--tenant", "acme", "--by", "ops", "--reason", "a\x07b"),
		Entry("tenant unhide with padded --by", "--by has leading or trailing spaces",
			"tenant", "unhide", "--tenant", "acme", "--by", "ops ", "--reason", "gone"),
		Entry("tenant delete with an extra argument", `unexpected argument "x"`,
			"tenant", "delete", "--tenant", "acme", "--by", "ops", "--reason", "gone", "x"),
		Entry("reviewer add without --reviewer", "admin reviewer add: --reviewer: reviewer is empty",
			"reviewer", "add", "--tenant", "acme", "--by", "ops", "--reason", "gone"),
		Entry("reviewer add with a padded --reviewer", "admin reviewer add: --reviewer: reviewer has leading or trailing spaces",
			"reviewer", "add", "--tenant", "acme", "--reviewer", " x", "--by", "ops", "--reason", "gone"),
		Entry("reviewer delete without --tenant", "admin reviewer delete: --tenant is required",
			"reviewer", "delete", "--reviewer", "alice", "--by", "ops", "--reason", "gone"),
		Entry("reviewer hide without --reason", "--reason is empty",
			"reviewer", "hide", "--tenant", "acme", "--reviewer", "alice", "--by", "ops"),
		Entry("reviewer list without --tenant", "admin reviewer list: --tenant is required", "reviewer", "list"),
		Entry("reviewer list with an extra argument", `unexpected argument "x"`, "reviewer", "list", "--tenant", "acme", "x"),
		Entry("identity trace with a bogus --pseudonym", "admin identity trace: --pseudonym: pseudonym must look like hidden-tenant-<16 hex>",
			"identity", "trace", "--pseudonym", "bogus", "--by", "ops", "--reason", "gone"),
		Entry("identity trace without --pseudonym", "admin identity trace: --pseudonym: pseudonym must look like",
			"identity", "trace", "--by", "ops", "--reason", "gone"),
		Entry("identity trace without --by", "--by is empty", "identity", "trace", "--pseudonym", testPseudonym, "--reason", "audit"),
		Entry("reviewer without a command", "usage:", "reviewer"),
		Entry("unknown reviewer command", "usage:", "reviewer", "purge"),
		Entry("identity without a command", "usage:", "identity"),
	)

	DescribeTable("returns 0 for -h",
		func(args ...string) {
			Expect(runAdmin(append(args, "-h"))).To(Equal(0))
		},
		Entry("tenant delete", "tenant", "delete"),
		Entry("tenant restore", "tenant", "restore"),
		Entry("tenant hide", "tenant", "hide"),
		Entry("tenant unhide", "tenant", "unhide"),
		Entry("reviewer add", "reviewer", "add"),
		Entry("reviewer delete", "reviewer", "delete"),
		Entry("reviewer restore", "reviewer", "restore"),
		Entry("reviewer hide", "reviewer", "hide"),
		Entry("reviewer unhide", "reviewer", "unhide"),
		Entry("reviewer list", "reviewer", "list"),
		Entry("identity trace", "identity", "trace"),
	)
})

var _ = Describe("runAdmin identity commands", func() {
	var (
		privPath, pubPath string
		fnErr             error
		seamCalls         int
		gotDSN            string
		gotReq            dbpkg.IdentityRequest
		gotSigned         dbpkg.SignedRequest
		gotTenant         string
		result            identityResult
		reviewers         []dbpkg.ReviewerRecord
		traced            dbpkg.TraceResult
	)
	audit := []string{"--by", "ops", "--reason", "gone"}
	traceArgs := []string{"identity", "trace", "--pseudonym", testPseudonym, "--by", "ops", "--reason", "audit request 42"}

	BeforeEach(func() {
		origAction, origList, origTrace := runIdentityActionFn, runReviewerListFn, runTraceFn
		fnErr, seamCalls, gotDSN, gotReq, gotSigned, gotTenant = nil, 0, "", dbpkg.IdentityRequest{}, dbpkg.SignedRequest{}, ""
		result, reviewers, traced = identityResult{}, nil, dbpkg.TraceResult{}
		runIdentityActionFn = func(_ context.Context, dsn string, req dbpkg.IdentityRequest, s dbpkg.SignedRequest) (identityResult, error) {
			seamCalls++
			gotDSN, gotReq, gotSigned = dsn, req, s
			return result, fnErr
		}
		runReviewerListFn = func(_ context.Context, dsn, tenantID string) ([]dbpkg.ReviewerRecord, error) {
			seamCalls++
			gotDSN, gotTenant = dsn, tenantID
			return reviewers, fnErr
		}
		runTraceFn = func(_ context.Context, dsn string, req dbpkg.IdentityRequest, s dbpkg.SignedRequest) (dbpkg.TraceResult, error) {
			seamCalls++
			gotDSN, gotReq, gotSigned = dsn, req, s
			return traced, fnErr
		}
		DeferCleanup(func() { runIdentityActionFn, runReviewerListFn, runTraceFn = origAction, origList, origTrace })

		privPath, pubPath = writeIdentityKeys(elliptic.P256())
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		GinkgoT().Setenv("CROSSCODEX_DATABASE_TENANT_ADMIN_DSN", testTenantAdminDSN)
		GinkgoT().Setenv("CROSSCODEX_ATTESTATION_PRIVATE_KEY_PATH", privPath)
		GinkgoT().Setenv("CROSSCODEX_ATTESTATION_PUBLIC_KEY_PATH", pubPath)
	})

	// verifySigned checks that s is req's canonical JSON signed as the single
	// material of an identity.<action> link, verifiable with the public key.
	verifySigned := func(s dbpkg.SignedRequest, want map[string]string) {
		var body map[string]string
		Expect(json.Unmarshal(s.Request, &body)).To(Succeed())
		for k, v := range want {
			Expect(body).To(HaveKeyWithValue(k, v))
		}
		at, err := time.Parse(time.RFC3339Nano, body["requested_at"])
		Expect(err).NotTo(HaveOccurred())
		Expect(at).To(BeTemporally("~", time.Now(), time.Minute))

		gen, err := attestation.NewGenerator(&attestation.FileKeyProvider{PrivateKeyPath: privPath, PublicKeyPath: pubPath})
		Expect(err).NotTo(HaveOccurred())
		link, err := gen.Verify(context.Background(), s.Attestation)
		Expect(err).NotTo(HaveOccurred())
		Expect(link.Step).To(Equal("identity." + want["action"]))
		sum := sha256.Sum256(s.Request)
		Expect(link.Materials).To(Equal([]attestation.Artifact{{URI: "crosscodex:identity-request", Digest: hex.EncodeToString(sum[:])}}))
	}

	// expectRequest checks the request the database seam received; its
	// RequestedAt must be set, and is otherwise checked by verifySigned.
	expectRequest := func(want dbpkg.IdentityRequest) {
		got := gotReq
		Expect(got.RequestedAt).NotTo(BeZero())
		got.RequestedAt = time.Time{}
		Expect(got).To(Equal(want))
	}

	DescribeTable("fails closed without attestation keys, before any database call",
		func(cmd string, args ...string) {
			GinkgoT().Setenv("CROSSCODEX_ATTESTATION_PRIVATE_KEY_PATH", "")
			GinkgoT().Setenv("CROSSCODEX_ATTESTATION_PUBLIC_KEY_PATH", "")
			code, stdout, stderr := runAdminCaptured(args...)
			Expect(code).To(Equal(1))
			Expect(stderr).To(ContainSubstring(cmd + " requires attestation keys because every identity change is signed. Set attestation.private_key_path and attestation.public_key_path."))
			Expect(stdout).To(BeEmpty())
			Expect(seamCalls).To(BeZero())
		},
		Entry("tenant delete", "admin tenant delete", "tenant", "delete", "--tenant", "acme", "--by", "ops", "--reason", "gone"),
		Entry("reviewer add", "admin reviewer add", "reviewer", "add", "--tenant", "acme", "--reviewer", "alice", "--by", "ops", "--reason", "gone"),
		Entry("identity trace", "admin identity trace", "identity", "trace", "--pseudonym", testPseudonym, "--by", "ops", "--reason", "audit request 42"),
	)

	It("refuses to sign with an unreadable key, before any database call", func() {
		missing := filepath.Join(GinkgoT().TempDir(), "missing.pem")
		GinkgoT().Setenv("CROSSCODEX_ATTESTATION_PRIVATE_KEY_PATH", missing)
		code, stdout, stderr := runAdminCaptured("tenant", "delete", "--tenant", "acme", "--by", "ops", "--reason", "gone")
		Expect(code).To(Equal(1))
		Expect(stderr).To(ContainSubstring("admin tenant delete: sign identity request:"))
		Expect(stderr).To(ContainSubstring(missing))
		Expect(stdout).To(BeEmpty())
		Expect(seamCalls).To(BeZero())
	})

	// P-224 loads and signs, but is not FIPS-approved, so only
	// tls.fips.enabled decides whether the change is signed.
	DescribeTable("signs only with a FIPS-approved key when tls.fips.enabled is set",
		func(fips bool) {
			priv, pub := writeIdentityKeys(elliptic.P224())
			GinkgoT().Setenv("CROSSCODEX_ATTESTATION_PRIVATE_KEY_PATH", priv)
			GinkgoT().Setenv("CROSSCODEX_ATTESTATION_PUBLIC_KEY_PATH", pub)
			GinkgoT().Setenv("CROSSCODEX_TLS_FIPS_ENABLED", fmt.Sprint(fips))
			code, _, stderr := runAdminCaptured("tenant", "delete", "--tenant", "acme", "--by", "ops", "--reason", "gone")
			if !fips {
				Expect(code).To(Equal(0), stderr)
				Expect(seamCalls).To(Equal(1))
				return
			}
			Expect(code).To(Equal(1))
			Expect(stderr).To(ContainSubstring("admin tenant delete: sign identity request: curve P-224 is not FIPS-approved"))
			Expect(seamCalls).To(BeZero())
		},
		Entry("enabled: refused before any database call", true),
		Entry("disabled: signed", false),
	)

	DescribeTable("refuses with the fix when database.tenant_admin_dsn is unset",
		func(args ...string) {
			GinkgoT().Setenv("CROSSCODEX_DATABASE_TENANT_ADMIN_DSN", "")
			code, stderr := captureStderr(func() int { return runAdmin(args) })
			Expect(code).To(Equal(1))
			Expect(stderr).To(ContainSubstring("database.tenant_admin_dsn is not set"))
			Expect(seamCalls).To(BeZero())
		},
		Entry("tenant delete", "tenant", "delete", "--tenant", "acme", "--by", "ops", "--reason", "gone"),
		Entry("reviewer list", "reviewer", "list", "--tenant", "acme"),
		Entry("identity trace", "identity", "trace", "--pseudonym", testPseudonym, "--by", "ops", "--reason", "audit request 42"),
	)

	DescribeTable("signs the request, passes it to the database and reports the result",
		func(kind, verb string, res identityResult, wantOut string) {
			result = res
			args := []string{kind, verb, "--tenant", "acme"}
			identity := "acme"
			if kind == dbpkg.IdentityKindReviewer {
				args, identity = append(args, "--reviewer", "alice"), "alice"
			}
			code, stdout, stderr := runAdminCaptured(append(args, audit...)...)
			Expect(code).To(Equal(0), stderr)
			Expect(stdout).To(Equal(wantOut + "\n"))
			Expect(gotDSN).To(Equal(testTenantAdminDSN))
			expectRequest(dbpkg.IdentityRequest{Action: verb, Kind: kind, TenantID: "acme", IdentityID: identity, Actor: "ops", Reason: "gone"})
			verifySigned(gotSigned, map[string]string{"action": verb, "kind": kind, "tenant_id": "acme",
				"identity_id": identity, "actor": "ops", "reason": "gone"})
		},
		Entry("tenant delete", "tenant", "delete", identityResult{}, "deleted"),
		Entry("tenant restore", "tenant", "restore", identityResult{},
			"restored; the tenant is suspended. Run 'crosscodexd admin tenant resume --tenant acme' to reactivate it"),
		Entry("tenant hide", "tenant", "hide", identityResult{Pseudonym: testPseudonym}, "hidden as "+testPseudonym),
		Entry("tenant unhide", "tenant", "unhide", identityResult{}, "unhidden"),
		Entry("reviewer add, new", "reviewer", "add", identityResult{Created: true}, "added"),
		Entry("reviewer add, already active", "reviewer", "add", identityResult{}, "already registered"),
		Entry("reviewer delete", "reviewer", "delete", identityResult{}, "deleted"),
		Entry("reviewer restore", "reviewer", "restore", identityResult{}, "restored"),
		Entry("reviewer hide", "reviewer", "hide", identityResult{Pseudonym: "hidden-reviewer-0123456789abcdef"},
			"hidden as hidden-reviewer-0123456789abcdef"),
		Entry("reviewer unhide", "reviewer", "unhide", identityResult{}, "unhidden"),
	)

	DescribeTable("exits 1 and prefixes the command on a database failure",
		func(prefix string, args ...string) {
			fnErr = errors.New("boom")
			code, stdout, stderr := runAdminCaptured(args...)
			Expect(code).To(Equal(1))
			Expect(stderr).To(ContainSubstring(prefix + ": boom"))
			Expect(stdout).To(BeEmpty())
		},
		Entry("tenant delete", "admin tenant delete", "tenant", "delete", "--tenant", "acme", "--by", "ops", "--reason", "gone"),
		Entry("reviewer hide", "admin reviewer hide", "reviewer", "hide", "--tenant", "acme", "--reviewer", "alice", "--by", "ops", "--reason", "gone"),
		Entry("reviewer list", "admin reviewer list", "reviewer", "list", "--tenant", "acme"),
		Entry("identity trace", "admin identity trace", "identity", "trace", "--pseudonym", testPseudonym, "--by", "ops", "--reason", "audit request 42"),
	)

	It("neutralizes control characters in a returned pseudonym", func() {
		result = identityResult{Pseudonym: "hidden\x1b[2J"}
		code, stdout, _ := runAdminCaptured("tenant", "hide", "--tenant", "acme", "--by", "ops", "--reason", "gone")
		Expect(code).To(Equal(0))
		Expect(stdout).NotTo(ContainSubstring("\x1b"))
	})

	Describe("identity trace", func() {
		It("signs a trace of the pseudonym and prints what it hides", func() {
			traced = dbpkg.TraceResult{Kind: "reviewer", TenantID: "acme", IdentityID: "alice", Status: "deleted", Hidden: true}
			code, stdout, stderr := runAdminCaptured(traceArgs...)
			Expect(code).To(Equal(0), stderr)
			expectRequest(dbpkg.IdentityRequest{Action: "trace", IdentityID: testPseudonym, Actor: "ops", Reason: "audit request 42"})
			verifySigned(gotSigned, map[string]string{"action": "trace", "kind": "", "tenant_id": "",
				"identity_id": testPseudonym, "actor": "ops", "reason": "audit request 42"})
			for _, line := range []string{`^FIELD +VALUE$`, `^kind +reviewer$`, `^tenant +acme$`, `^identity +alice$`,
				`^status +deleted$`, `^hidden +yes$`} {
				Expect(stdout).To(MatchRegexp(`(?m)` + line))
			}
		})

		It("neutralizes control and bidi characters in the traced identity", func() {
			traced = dbpkg.TraceResult{Kind: "reviewer", TenantID: "acme", IdentityID: "al\x1b[2J\u202eice", Status: "deleted"}
			code, stdout, _ := runAdminCaptured(traceArgs...)
			Expect(code).To(Equal(0))
			Expect(stdout).NotTo(ContainSubstring("\x1b"))
			Expect(stdout).NotTo(ContainSubstring("\u202e"))
			Expect(stdout).To(MatchRegexp(`(?m)^hidden +no$`))
		})
	})

	Describe("reviewer list", func() {
		created := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
		deleted := time.Date(2026, 10, 9, 8, 30, 0, 0, time.UTC)

		It("prints one row per reviewer without signing", func() {
			GinkgoT().Setenv("CROSSCODEX_ATTESTATION_PRIVATE_KEY_PATH", "")
			GinkgoT().Setenv("CROSSCODEX_ATTESTATION_PUBLIC_KEY_PATH", "")
			reviewers = []dbpkg.ReviewerRecord{
				{ID: "alice", Status: "active", CreatedAt: created, CreatedBy: "ops"},
				{ID: "hidden-reviewer-0123456789abcdef", Status: "deleted", CreatedAt: created, CreatedBy: "ops", DeletedAt: &deleted, Hidden: true},
			}
			code, stdout, stderr := runAdminCaptured("reviewer", "list", "--tenant", "acme")
			Expect(code).To(Equal(0), stderr)
			Expect(gotDSN).To(Equal(testTenantAdminDSN))
			Expect(gotTenant).To(Equal("acme"))
			lines := strings.Split(strings.TrimSpace(stdout), "\n")
			Expect(lines).To(HaveLen(3))
			Expect(strings.Fields(lines[0])).To(Equal([]string{"REVIEWER", "STATUS", "CREATED", "BY", "DELETED", "HIDDEN"}))
			Expect(strings.Fields(lines[1])).To(Equal([]string{"alice", "active", "2026-10-08T12:00:00Z", "ops", "-", "no"}))
			Expect(strings.Fields(lines[2])).To(Equal([]string{"hidden-reviewer-0123456789abcdef", "deleted", "2026-10-08T12:00:00Z", "ops",
				"2026-10-09T08:30:00Z", "yes"}))
		})

		It("says so when the tenant has no reviewers", func() {
			code, stdout, _ := runAdminCaptured("reviewer", "list", "--tenant", "acme")
			Expect(code).To(Equal(0))
			Expect(stdout).To(Equal("no reviewers\n"))
		})

		It("neutralizes control and bidi characters in stored values", func() {
			reviewers = []dbpkg.ReviewerRecord{{ID: "al\x1b[2J\u202eice", Status: "active", CreatedAt: created, CreatedBy: "o\tps"}}
			code, stdout, _ := runAdminCaptured("reviewer", "list", "--tenant", "acme")
			Expect(code).To(Equal(0))
			Expect(stdout).NotTo(ContainSubstring("\x1b"))
			Expect(stdout).NotTo(ContainSubstring("\u202e"))
			Expect(stdout).NotTo(ContainSubstring("\t"))
		})
	})
})

var _ = Describe("runAdmin identity export", func() {
	var (
		seamCalls int
		gotDSN    string
		events    []dbpkg.IdentityEvent
		failAfter error
		dir, out  string
	)
	occurred := time.Date(2026, 10, 14, 9, 30, 0, 123456000, time.FixedZone("CEST", 2*60*60))

	BeforeEach(func() {
		orig := runIdentityExportFn
		seamCalls, gotDSN, failAfter = 0, "", nil
		events = []dbpkg.IdentityEvent{
			{EventID: 7, OccurredAt: occurred, Action: "delete", Kind: "tenant", TenantID: "acme", IdentityID: "acme",
				Actor: "ops", DBUser: "tenant_admin", Reason: "offboarded", Request: []byte(`{"action":"delete"}`),
				Attestation: []byte{0x00, 0xff, '\n'}, AttestationSHA256: "abc"},
			{EventID: 9, OccurredAt: occurred, Action: "trace", Kind: "reviewer", TenantID: "acme", IdentityID: "alice",
				Actor: "ops", DBUser: "tenant_admin", Reason: "audit", Request: []byte(`{}`), Attestation: []byte(`{}`), AttestationSHA256: "def"},
		}
		// The seam streams events, then fails with failAfter if it is set,
		// as a lost connection mid-export would.
		runIdentityExportFn = func(_ context.Context, dsn string, fn func(dbpkg.IdentityEvent) error) error {
			seamCalls++
			gotDSN = dsn
			for _, e := range events {
				if err := fn(e); err != nil {
					return err
				}
			}
			return failAfter
		}
		DeferCleanup(func() { runIdentityExportFn = orig })
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		GinkgoT().Setenv("CROSSCODEX_DATABASE_TENANT_ADMIN_DSN", testTenantAdminDSN)
		dir = GinkgoT().TempDir()
		out = filepath.Join(dir, "audit.jsonl")
	})

	// dirEntries lists dir's file names, to prove nothing else was left.
	dirEntries := func() []string {
		entries, err := os.ReadDir(dir)
		Expect(err).NotTo(HaveOccurred())
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return names
	}

	DescribeTable("returns 2 with an actionable message for usage errors, before any call",
		func(want string, args ...string) {
			code, stderr := captureStderr(func() int { return runAdmin(append([]string{"identity", "export"}, args...)) })
			Expect(code).To(Equal(2))
			Expect(stderr).To(ContainSubstring(want))
			Expect(seamCalls).To(BeZero())
			Expect(dirEntries()).To(BeEmpty())
		},
		Entry("without --out", "admin identity export: --out is required; name a file that does not exist yet"),
		Entry("with an extra argument", `unexpected argument "x"`, "--out", "audit.jsonl", "x"),
	)

	It("returns 0 for -h and documents that the file is sensitive", func() {
		code, stderr := captureStderr(func() int { return runAdmin([]string{"identity", "export", "-h"}) })
		Expect(code).To(Equal(0))
		Expect(stderr).To(ContainSubstring("real IDs of hidden tenants and reviewers"))
		Expect(stderr).To(ContainSubstring("store it securely"))
	})

	It("lists the command in the admin usage", func() {
		Expect(adminUsage).To(ContainSubstring("crosscodexd admin identity export --out <path>"))
	})

	It("refuses with the fix when database.tenant_admin_dsn is unset, writing nothing", func() {
		GinkgoT().Setenv("CROSSCODEX_DATABASE_TENANT_ADMIN_DSN", "")
		code, stdout, stderr := runAdminCaptured("identity", "export", "--out", out)
		Expect(code).To(Equal(1))
		Expect(stderr).To(ContainSubstring("database.tenant_admin_dsn is not set"))
		Expect(stdout).To(BeEmpty())
		Expect(seamCalls).To(BeZero())
		Expect(dirEntries()).To(BeEmpty())
	})

	DescribeTable("refuses to overwrite, leaving the existing file unchanged and reading nothing",
		func(existing, want string) {
			path := filepath.Join(dir, existing)
			Expect(os.WriteFile(path, []byte("earlier export\n"), 0o640)).To(Succeed())
			code, stdout, stderr := runAdminCaptured("identity", "export", "--out", out)
			Expect(code).To(Equal(1))
			Expect(stderr).To(ContainSubstring(want))
			Expect(stdout).To(BeEmpty())
			Expect(seamCalls).To(BeZero())
			Expect(dirEntries()).To(Equal([]string{existing}))
			body, err := os.ReadFile(path)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(body)).To(Equal("earlier export\n"))
			info, err := os.Stat(path)
			Expect(err).NotTo(HaveOccurred())
			Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o640)))
		},
		Entry("the --out file", "audit.jsonl", "audit.jsonl already exists; nothing was written. Choose a new --out path"),
		Entry("a partial file from an interrupted export", "audit.jsonl.partial",
			"audit.jsonl.partial already exists, probably left by an interrupted export, and is incomplete; nothing was written. Remove it and run the export again"),
	)

	It("refuses a --out in a missing directory with the fix", func() {
		missing := filepath.Join(dir, "nope", "audit.jsonl")
		code, _, stderr := runAdminCaptured("identity", "export", "--out", missing)
		Expect(code).To(Equal(1))
		Expect(stderr).To(ContainSubstring("Check that the directory exists and is writable"))
		Expect(dirEntries()).To(BeEmpty())
	})

	It("removes the partial file and leaves no --out file when the export fails midway", func() {
		failAfter = errors.New("connection lost")
		code, stdout, stderr := runAdminCaptured("identity", "export", "--out", out)
		Expect(code).To(Equal(1))
		Expect(stderr).To(ContainSubstring("admin identity export: connection lost"))
		Expect(stdout).To(BeEmpty())
		Expect(seamCalls).To(Equal(1))
		Expect(dirEntries()).To(BeEmpty())
	})

	It("writes one JSON object per event to a 0600 file, then reports the count", func() {
		code, stdout, stderr := runAdminCaptured("identity", "export", "--out", out)
		Expect(code).To(Equal(0), stderr)
		Expect(gotDSN).To(Equal(testTenantAdminDSN))
		Expect(stdout).To(Equal(fmt.Sprintf("exported 2 identity events to %s\n", out)))
		Expect(dirEntries()).To(Equal([]string{"audit.jsonl"}))
		info, err := os.Stat(out)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)))

		body, err := os.ReadFile(out)
		Expect(err).NotTo(HaveOccurred())
		lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
		Expect(lines).To(HaveLen(2))
		var first map[string]any
		Expect(json.Unmarshal([]byte(lines[0]), &first)).To(Succeed())
		Expect(first).To(Equal(map[string]any{
			"event_id": float64(7), "occurred_at": "2026-10-14T07:30:00.123456Z", "action": "delete", "kind": "tenant",
			"tenant_id": "acme", "identity_id": "acme", "actor": "ops", "db_user": "tenant_admin", "reason": "offboarded",
			"request": "eyJhY3Rpb24iOiJkZWxldGUifQ==", "attestation": "AP8K", "attestation_sha256": "abc",
		}))
		var second map[string]any
		Expect(json.Unmarshal([]byte(lines[1]), &second)).To(Succeed())
		Expect(second).To(HaveKeyWithValue("event_id", float64(9)))
		Expect(second).To(HaveKeyWithValue("identity_id", "alice"))
	})

	It("writes an empty file for an empty audit log", func() {
		events = nil
		code, stdout, stderr := runAdminCaptured("identity", "export", "--out", out)
		Expect(code).To(Equal(0), stderr)
		Expect(stdout).To(Equal(fmt.Sprintf("exported 0 identity events to %s\n", out)))
		body, err := os.ReadFile(out)
		Expect(err).NotTo(HaveOccurred())
		Expect(body).To(BeEmpty())
	})
})
