package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/testspecs"
	"github.com/complytime-labs/crosscodex/pkg/db"
)

func TestDBBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Database Package BDD Suite")
}

// Redirect slog output to GinkgoWriter so log noise only appears on failure.
var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

var _ = Describe("Database Package", Ordered, func() {

	BeforeAll(func() {
		testspecs.LogTestProgress("Starting Database Package BDD test suite")
	})

	AfterAll(func() {
		testspecs.LogTestProgress("Database Package BDD test suite completed")
	})

	// =================================================================
	// LEVEL 1: BEHAVIORAL SPECIFICATIONS
	// These specs test the "why" - what business behaviors the db package supports
	// =================================================================

	Describe("Connection Pool Behaviors", func() {
		Context("when protecting credentials in error paths", func() {
			It("never leaks database passwords in connection errors", func() {
				By("attempting to connect with a password-bearing DSN to an unreachable host")
				password := "s3cret-passw0rd!"
				dsn := "postgres://admin:" + password + "@unreachable-host:5432/mydb?sslmode=disable" // DevSkim: ignore DS162092 — test fixture

				_, err := db.NewPool(db.PoolConfig{DSN: dsn})
				Expect(err).To(HaveOccurred())

				By("verifying the password is absent from the error message")
				errMsg := err.Error()
				Expect(errMsg).NotTo(ContainSubstring(password))

				By("verifying the error contains a redaction marker")
				Expect(errMsg).To(ContainSubstring("REDACTED"))
			})

			It("rejects syntactically invalid DSNs with a clear error", func() {
				By("passing a non-URI, non-keyword=value DSN")
				_, err := db.NewPool(db.PoolConfig{DSN: "not-a-valid-dsn"})
				Expect(err).To(HaveOccurred())
			})
		})

		Context("when configuring pool parameters", func() {
			It("constructs a pool configuration from discrete parameters", func() {
				By("creating a config with explicit values")
				cfg := db.NewPoolConfigFrom(
					"postgres://localhost/test",       // DevSkim: ignore DS162092 — test fixture
					"postgres://localhost/test_graph", // DevSkim: ignore DS162092 — test fixture
					20, "require", []string{"age", "vector"},
				)

				By("verifying all fields are populated correctly")
				Expect(cfg.DSN).To(Equal("postgres://localhost/test"))            // DevSkim: ignore DS162092 — test fixture
				Expect(cfg.GraphDSN).To(Equal("postgres://localhost/test_graph")) // DevSkim: ignore DS162092 — test fixture
				Expect(cfg.MaxOpenConns).To(Equal(20))
				Expect(cfg.SSLMode).To(Equal("require"))
				Expect(cfg.Extensions).To(HaveLen(2))
				Expect(cfg.Extensions).To(ConsistOf("age", "vector"))
			})

			It("applies functional options to override defaults", func() {
				By("creating a pool config with custom idle conns via option")
				cfg := db.PoolConfig{
					DSN:          "postgres://admin:pass@unreachable-host:5432/mydb?sslmode=disable", // DevSkim: ignore DS162092 — test fixture
					MaxOpenConns: 10,
				}

				// WithMaxIdleConns and WithConnMaxLifetime are exported options;
				// we verify they compile and don't panic when applied to NewPool.
				// Actual connection will fail (unreachable host), but options are applied.
				_, err := db.NewPool(cfg, db.WithMaxIdleConns(10), db.WithConnMaxLifetime(5*time.Minute))
				Expect(err).To(HaveOccurred()) // unreachable host
			})
		})
	})

	Describe("Tenant Context Propagation Behaviors", func() {
		Context("when enforcing tenant-scoped database access", func() {
			It("propagates tenant identity through context for RLS enforcement", func() {
				By("setting a tenant ID in context")
				ctx := db.ContextWithTenant(context.Background(), "acme-corp")

				By("extracting the tenant ID downstream")
				tenantID := db.TenantFromContext(ctx)
				Expect(tenantID).To(Equal("acme-corp"))
			})

			It("propagates user identity through context for job ownership", func() {
				By("setting a user ID in context")
				ctx := db.ContextWithUser(context.Background(), "alice")

				By("extracting the user ID downstream")
				userID := db.UserFromContext(ctx)
				Expect(userID).To(Equal("alice"))
			})

			It("prevents non-transactional queries to enforce RLS boundaries", func() {
				// TenantPool rejects Query/Exec/QueryRow outside transactions
				// because SET LOCAL (for RLS) has no effect without a transaction.
				tp := db.NewTenantPool(nil)

				By("rejecting Query without a transaction")
				_, err := tp.Query(context.Background(), "SELECT 1")
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, db.ErrTenantRequired)).To(BeTrue())

				By("rejecting Exec without a transaction")
				err = tp.Exec(context.Background(), "SELECT 1")
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, db.ErrTenantRequired)).To(BeTrue())

				By("rejecting QueryRow without a transaction")
				row := tp.QueryRow(context.Background(), "SELECT 1")
				err = row.Scan()
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, db.ErrTenantRequired)).To(BeTrue())
			})

			It("requires tenant context to begin a transaction", func() {
				tp := db.NewTenantPool(nil)

				By("attempting Begin without tenant in context")
				_, err := tp.Begin(context.Background())
				Expect(err).To(HaveOccurred())
				Expect(errors.Is(err, db.ErrTenantRequired)).To(BeTrue())
			})

			It("maintains independent tenant and user contexts", func() {
				By("setting both tenant and user on the same context")
				ctx := db.ContextWithTenant(context.Background(), "tenant-a")
				ctx = db.ContextWithUser(ctx, "user-bob")

				By("extracting each independently")
				Expect(db.TenantFromContext(ctx)).To(Equal("tenant-a"))
				Expect(db.UserFromContext(ctx)).To(Equal("user-bob"))
			})
		})
	})

	// =================================================================
	// LEVEL 2: INTERFACE COMPLIANCE SPECIFICATIONS
	// These specs verify that db types satisfy CrossCodex interface contracts.
	// The db package's tenant context helpers use a private context key
	// (tenantKey{}) distinct from pkg/tenant, so TenantIsolatedComponent
	// adaptation is not applicable here. The tenant_pool enforces
	// isolation via ErrTenantRequired on non-transactional operations,
	// which is covered in Level 1 and Level 3 specs.
	// =================================================================

	Describe("Interface Compliance", func() {
		Context("TenantConnection contract", func() {
			It("satisfies the Connection interface through NewTenantPool", func() {
				// NewTenantPool returns TenantConnection which embeds Connection.
				// Verify the returned value implements Connection at compile time.
				var conn db.TenantConnection = db.NewTenantPool(nil)
				Expect(conn).NotTo(BeNil())

				// Also verify it is usable as a Connection
				var _ db.Connection = conn
			})
		})
	})

	// =================================================================
	// LEVEL 3: TECHNICAL EDGE CASES
	// Comprehensive coverage of edge cases from pool_test.go and tenant_pool_test.go
	// =================================================================

	Describe("Pool Configuration Edge Cases", func() {
		Context("when constructing PoolConfig via NewPoolConfigFrom", func() {
			It("stores all provided parameters", func() {
				cfg := db.NewPoolConfigFrom(
					"postgres://localhost/db1",       // DevSkim: ignore DS162092 — test fixture
					"postgres://localhost/db1_graph", // DevSkim: ignore DS162092 — test fixture
					50, "verify-full", []string{"pgcrypto"},
				)

				Expect(cfg.DSN).To(Equal("postgres://localhost/db1"))            // DevSkim: ignore DS162092 — test fixture
				Expect(cfg.GraphDSN).To(Equal("postgres://localhost/db1_graph")) // DevSkim: ignore DS162092 — test fixture
				Expect(cfg.MaxOpenConns).To(Equal(50))
				Expect(cfg.SSLMode).To(Equal("verify-full"))
				Expect(cfg.Extensions).To(Equal([]string{"pgcrypto"}))
			})

			It("handles zero extensions gracefully", func() {
				cfg := db.NewPoolConfigFrom(
					"postgres://localhost/db2",       // DevSkim: ignore DS162092 — test fixture
					"postgres://localhost/db2_graph", // DevSkim: ignore DS162092 — test fixture
					5, "disable", nil,
				)
				Expect(cfg.Extensions).To(BeNil())
			})

			It("handles empty extensions slice", func() {
				cfg := db.NewPoolConfigFrom(
					"postgres://localhost/db3",       // DevSkim: ignore DS162092 — test fixture
					"postgres://localhost/db3_graph", // DevSkim: ignore DS162092 — test fixture
					5, "disable", []string{},
				)
				Expect(cfg.Extensions).To(BeEmpty())
			})
		})

		Context("when creating a pool with invalid configuration", func() {
			It("returns an error for a non-URI DSN", func() {
				_, err := db.NewPool(db.PoolConfig{DSN: "not-a-valid-dsn"})
				Expect(err).To(HaveOccurred())
			})

			It("returns an error for an empty DSN", func() {
				_, err := db.NewPool(db.PoolConfig{DSN: ""})
				Expect(err).To(HaveOccurred())
			})

			It("redacts URI-style passwords in error output", func() {
				password := "hunter2"
				dsn := "postgres://user:" + password + "@unreachable:5432/db?sslmode=disable" // DevSkim: ignore DS162092 — test fixture
				_, err := db.NewPool(db.PoolConfig{DSN: dsn})
				Expect(err).To(HaveOccurred())

				errMsg := err.Error()
				Expect(errMsg).NotTo(ContainSubstring(password))
				Expect(errMsg).To(ContainSubstring("REDACTED"))
			})

			It("does not leak passwords from keyword=value DSNs in errors", func() {
				// Keyword=value DSNs go through redactDSN internally.
				// Since NewPool parses via pgx which expects URI format,
				// a kv-style DSN will fail parsing, but the error path
				// should still redact. We verify the exported behavior.
				dsn := "host=unreachable port=5432 user=admin password=topsecret dbname=mydb sslmode=disable"
				_, err := db.NewPool(db.PoolConfig{DSN: dsn})
				Expect(err).To(HaveOccurred())

				errMsg := err.Error()
				Expect(errMsg).NotTo(ContainSubstring("topsecret"))
			})
		})

		Context("when applying functional options", func() {
			It("accepts WithMaxIdleConns option", func() {
				opt := db.WithMaxIdleConns(20)
				Expect(opt).NotTo(BeNil())
			})

			It("accepts WithConnMaxLifetime option", func() {
				opt := db.WithConnMaxLifetime(10 * time.Minute)
				Expect(opt).NotTo(BeNil())
			})
		})
	})

	Describe("Tenant Context Edge Cases", func() {
		Context("when extracting tenant from context", func() {
			It("returns empty string for a bare context", func() {
				ctx := context.Background()
				Expect(db.TenantFromContext(ctx)).To(BeEmpty())
			})

			It("returns the set tenant ID", func() {
				ctx := db.ContextWithTenant(context.Background(), "acme")
				Expect(db.TenantFromContext(ctx)).To(Equal("acme"))
			})

			It("returns empty for a context without tenant key", func() {
				// A context with an unrelated key should not confuse TenantFromContext
				type unrelatedKey string
				ctx := context.WithValue(context.Background(), unrelatedKey("unrelated"), "value")
				Expect(db.TenantFromContext(ctx)).To(BeEmpty())
			})
		})

		Context("when extracting user from context", func() {
			It("returns empty string for a bare context", func() {
				ctx := context.Background()
				Expect(db.UserFromContext(ctx)).To(BeEmpty())
			})

			It("returns the set user ID", func() {
				ctx := db.ContextWithUser(context.Background(), "alice")
				Expect(db.UserFromContext(ctx)).To(Equal("alice"))
			})
		})

		Context("when stacking tenant and user context values", func() {
			It("preserves both values when layered", func() {
				ctx := db.ContextWithTenant(context.Background(), "tenant-x")
				ctx = db.ContextWithUser(ctx, "user-y")

				Expect(db.TenantFromContext(ctx)).To(Equal("tenant-x"))
				Expect(db.UserFromContext(ctx)).To(Equal("user-y"))
			})

			It("allows overwriting tenant on the same context chain", func() {
				ctx := db.ContextWithTenant(context.Background(), "first")
				ctx = db.ContextWithTenant(ctx, "second")

				Expect(db.TenantFromContext(ctx)).To(Equal("second"))
			})
		})
	})

	Describe("TenantPool Operation Rejection Edge Cases", func() {
		var tp db.TenantConnection

		BeforeEach(func() {
			// nil underlying pool is fine because Query/Exec/QueryRow
			// reject before touching it, and Begin checks tenant first.
			tp = db.NewTenantPool(nil)
		})

		Context("when calling Query on TenantPool", func() {
			It("returns ErrTenantRequired", func() {
				_, err := tp.Query(context.Background(), "SELECT 1")
				Expect(err).To(MatchError(db.ErrTenantRequired))
			})
		})

		Context("when calling Exec on TenantPool", func() {
			It("returns ErrTenantRequired", func() {
				err := tp.Exec(context.Background(), "SELECT 1")
				Expect(err).To(MatchError(db.ErrTenantRequired))
			})
		})

		Context("when calling QueryRow on TenantPool", func() {
			It("returns ErrTenantRequired on Scan", func() {
				row := tp.QueryRow(context.Background(), "SELECT 1")
				err := row.Scan()
				Expect(err).To(MatchError(db.ErrTenantRequired))
			})

			It("returns ErrTenantRequired regardless of Scan arguments", func() {
				row := tp.QueryRow(context.Background(), "SELECT 1")
				var a, b string
				err := row.Scan(&a, &b)
				Expect(err).To(MatchError(db.ErrTenantRequired))
			})
		})

		Context("when calling Begin on TenantPool without tenant", func() {
			It("returns ErrTenantRequired", func() {
				_, err := tp.Begin(context.Background())
				Expect(err).To(MatchError(db.ErrTenantRequired))
			})
		})
	})

	Describe("Credential Redaction Edge Cases", func() {
		// These specs exercise redactDSN indirectly through NewPool error paths.
		// Direct redactDSN coverage is in the "Internal redactDSN Function" section.

		DescribeTable("password is never present in NewPool error messages",
			func(dsn string, password string) {
				_, err := db.NewPool(db.PoolConfig{DSN: dsn})
				Expect(err).To(HaveOccurred())

				errMsg := err.Error()
				if password != "" {
					Expect(errMsg).NotTo(ContainSubstring(password),
						"error message should not contain password %q", password)
				}
			},
			Entry("URI with password",
				"postgres://user:secret@unreachable:5432/db?sslmode=disable", // DevSkim: ignore DS162092 — test fixture
				"secret"),
			Entry("URI without password",
				"postgres://user@unreachable:5432/db", // DevSkim: ignore DS162092 — test fixture
				""),
			Entry("keyword=value with password",
				"host=unreachable port=5432 user=admin password=secret dbname=mydb sslmode=disable",
				"secret"),
			Entry("keyword=value with quoted password",
				"host=unreachable password='my secret' dbname=mydb",
				"my secret"),
			Entry("non-parseable DSN",
				"not-a-valid-dsn",
				""),
			Entry("empty DSN",
				"",
				""),
		)
	})

	Describe("Error Sentinel Values", func() {
		It("exposes distinct sentinel errors for different failure modes", func() {
			// Verify sentinel errors are distinct and non-nil
			sentinels := []error{
				db.ErrNoRows,
				db.ErrTxDone,
				db.ErrConnClosed,
				db.ErrTenantRequired,
				db.ErrExtensionMissing,
				db.ErrMigrationDirty,
				db.ErrPoolNotReady,
				db.ErrImmutableRecord,
			}

			By("checking all sentinels are non-nil")
			for _, s := range sentinels {
				Expect(s).NotTo(BeNil())
			}

			By("checking all sentinels have distinct messages")
			seen := make(map[string]bool)
			for _, s := range sentinels {
				msg := s.Error()
				Expect(msg).NotTo(BeEmpty())
				Expect(seen[msg]).To(BeFalse(), "duplicate sentinel message: %s", msg)
				seen[msg] = true
			}
		})

		It("wraps ErrTenantRequired correctly through TenantPool", func() {
			tp := db.NewTenantPool(nil)
			_, err := tp.Query(context.Background(), "SELECT 1")
			Expect(errors.Is(err, db.ErrTenantRequired)).To(BeTrue())
		})
	})

	Describe("ExtensionError Behavior", func() {
		It("formats missing extensions in the error message", func() {
			extErr := &db.ExtensionError{Missing: []string{"age", "vector"}}
			msg := extErr.Error()
			Expect(msg).To(ContainSubstring("age"))
			Expect(msg).To(ContainSubstring("vector"))
		})

		It("unwraps to ErrExtensionMissing sentinel", func() {
			extErr := &db.ExtensionError{Missing: []string{"pgcrypto"}}
			Expect(errors.Is(extErr, db.ErrExtensionMissing)).To(BeTrue())
		})
	})

	Describe("PoolConfig Struct Edge Cases", func() {
		It("supports all configuration fields including optional ones", func() {
			cfg := db.PoolConfig{
				DSN:          "postgres://localhost/db",  // DevSkim: ignore DS162092 — test fixture
				GraphDSN:     "postgres://localhost/dbg", // DevSkim: ignore DS162092 — test fixture
				MaxOpenConns: 25,
				MaxIdleConns: 10,
				ConnMaxLife:  15 * time.Minute,
				SSLMode:      "verify-ca",
				Extensions:   []string{"age", "vector", "pgcrypto"},
			}

			Expect(cfg.DSN).NotTo(BeEmpty())
			Expect(cfg.GraphDSN).NotTo(BeEmpty())
			Expect(cfg.MaxOpenConns).To(Equal(25))
			Expect(cfg.MaxIdleConns).To(Equal(10))
			Expect(cfg.ConnMaxLife).To(Equal(15 * time.Minute))
			Expect(cfg.SSLMode).To(Equal("verify-ca"))
			Expect(cfg.Extensions).To(HaveLen(3))
		})

		It("has zero values for unset fields", func() {
			cfg := db.PoolConfig{}
			Expect(cfg.DSN).To(BeEmpty())
			Expect(cfg.MaxOpenConns).To(BeZero())
			Expect(cfg.Extensions).To(BeNil())
		})
	})

	Describe("Security: DSN Password Redaction via NewPool", func() {
		It("redacts passwords in URI-style DSNs", func() {
			dsn := "postgres://user:supersecret@unreachable:5432/db?sslmode=disable" // DevSkim: ignore DS162092 — test fixture
			_, err := db.NewPool(db.PoolConfig{DSN: dsn})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).NotTo(ContainSubstring("supersecret"))
			Expect(err.Error()).To(ContainSubstring("REDACTED"))
		})

		It("leaves URI-style DSNs without passwords unmodified in errors", func() {
			dsn := "postgres://user@unreachable:5432/db" // DevSkim: ignore DS162092 — test fixture
			_, err := db.NewPool(db.PoolConfig{DSN: dsn})
			Expect(err).To(HaveOccurred())
			// No REDACTED because there's no password to redact
			Expect(err.Error()).To(ContainSubstring("user"))
		})

		It("redacts passwords in keyword=value DSNs", func() {
			dsn := "host=unreachable port=5432 user=admin password=secret dbname=mydb sslmode=disable"
			_, err := db.NewPool(db.PoolConfig{DSN: dsn})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).NotTo(ContainSubstring("secret"))
		})

		It("redacts quoted passwords in keyword=value DSNs", func() {
			dsn := "host=unreachable password='my secret' dbname=mydb"
			_, err := db.NewPool(db.PoolConfig{DSN: dsn})
			Expect(err).To(HaveOccurred())

			errMsg := err.Error()
			Expect(errMsg).NotTo(ContainSubstring("my secret"))
		})

		It("handles unparseable DSNs without leaking content", func() {
			dsn := "not-a-valid-dsn"
			_, err := db.NewPool(db.PoolConfig{DSN: dsn})
			Expect(err).To(HaveOccurred())
			// The error should reference the unparseable marker or generic message
			_ = err.Error() // should not panic
		})
	})

	// =================================================================
	// LEVEL 4: INTERNAL FUNCTION SPECIFICATIONS
	// These specs exercise unexported functions through export_test.go
	// to ensure complete coverage of internal logic.
	// =================================================================

	Describe("Internal Pool Options", func() {
		Context("when using default options", func() {
			It("sets maxIdleConns to 5", func() {
				opts := db.ExportDefaultOptions()
				Expect(opts.MaxIdleConns).To(Equal(5))
			})

			It("sets connMaxLife to 30 minutes", func() {
				opts := db.ExportDefaultOptions()
				Expect(opts.ConnMaxLife).To(Equal(30 * time.Minute))
			})
		})

		Context("when applying WithMaxIdleConns", func() {
			It("overrides the default maxIdleConns value", func() {
				opts := db.ExportApplyOption(db.WithMaxIdleConns(10))
				Expect(opts.MaxIdleConns).To(Equal(10))
			})

			It("does not change connMaxLife", func() {
				opts := db.ExportApplyOption(db.WithMaxIdleConns(10))
				Expect(opts.ConnMaxLife).To(Equal(30 * time.Minute))
			})
		})

		Context("when applying WithConnMaxLifetime", func() {
			It("overrides the default connMaxLife value", func() {
				opts := db.ExportApplyOption(db.WithConnMaxLifetime(5 * time.Minute))
				Expect(opts.ConnMaxLife).To(Equal(5 * time.Minute))
			})

			It("does not change maxIdleConns", func() {
				opts := db.ExportApplyOption(db.WithConnMaxLifetime(5 * time.Minute))
				Expect(opts.MaxIdleConns).To(Equal(5))
			})
		})
	})

	Describe("Internal redactDSN Function", func() {
		DescribeTable("redacts passwords correctly across DSN formats",
			func(dsn string, expected string, notContain string) {
				got := db.ExportRedactDSN(dsn)
				Expect(got).To(Equal(expected))
				if notContain != "" {
					Expect(got).NotTo(ContainSubstring(notContain))
				}
			},
			Entry("URI with password",
				"postgres://user:secret@localhost:5432/db?sslmode=disable",
				"postgres://user:REDACTED@localhost:5432/db?sslmode=disable",
				"secret"),
			Entry("URI without password",
				"postgres://user@localhost:5432/db", // DevSkim: ignore DS162092 — test fixture
				"postgres://user@localhost:5432/db",
				""),
			Entry("unparseable DSN",
				"not-a-valid-dsn",
				"<unparseable-dsn>",
				""),
			Entry("empty DSN",
				"",
				"<unparseable-dsn>",
				""),
			Entry("keyword=value with password",
				"host=localhost port=5432 user=admin password=secret dbname=mydb sslmode=disable",
				"host=localhost port=5432 user=admin password=REDACTED dbname=mydb sslmode=disable",
				"secret"),
			Entry("keyword=value without password",
				"host=localhost port=5432 user=admin dbname=mydb sslmode=disable",
				"host=localhost port=5432 user=admin dbname=mydb sslmode=disable",
				""),
			Entry("keyword=value with quoted password",
				"host=localhost password='my secret' dbname=mydb",
				"host=localhost password=REDACTED dbname=mydb",
				"my secret"),
		)
	})

	Describe("Pool Close Idempotency", func() {
		It("returns nil when closing an already-closed pool", func() {
			pool := db.ExportNewClosedPool()
			err := pool.Close()
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("errRow Internal Behavior", func() {
		It("returns the stored error from Scan regardless of arguments", func() {
			row := db.ExportNewErrRow(db.ErrTenantRequired)
			err := row.Scan("a", "b")
			Expect(errors.Is(err, db.ErrTenantRequired)).To(BeTrue())
		})

		It("returns the stored error from Scan with no arguments", func() {
			row := db.ExportNewErrRow(db.ErrTenantRequired)
			err := row.Scan()
			Expect(errors.Is(err, db.ErrTenantRequired)).To(BeTrue())
		})
	})

})
