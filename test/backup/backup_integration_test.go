//go:build integration_backup

// Package backup_test drives test/compose.backup.yaml end to end: crosscodexd
// admin backup in the crosscodexd image, WAL-G archiving and
// crosscodex-db-restore in the db image, rustfs, and external NATS. Run it
// with `task test:integration:backup`.
package backup_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/storage"
)

func TestBackupIntegration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Backup Integration Suite")
}

// Must match test/compose.backup.yaml.
const (
	project       = "crosscodex-backup-test"
	dbContainer   = "crosscodex-backup-test-db"
	s3Endpoint    = "http://localhost:29000"
	natsURL       = "tls://localhost:24222"
	objectsBucket = "crosscodex-objects"
	backupBucket  = "crosscodex-backup"
)

var (
	tenants    = []string{"acme-corp", "globex"}
	pointRe    = regexp.MustCompile(`backup point (\d{8}T\d{6}Z-[0-9a-f]{8}) committed`)
	walgNameRe = regexp.MustCompile(`base_[0-9A-F]{24}`)

	composeCmd             []string
	engine, composeFile    string
	suDSN, s3Key, s3Secret string
	natsTLS                *tls.Config
)

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		Fail(k + " is not set — run: task test:integration:backup")
	}
	return v
}

var _ = BeforeSuite(func() {
	composeCmd = strings.Fields(os.Getenv("TEST_COMPOSE_CMD"))
	if len(composeCmd) == 0 {
		Skip("TEST_COMPOSE_CMD not set — run: task test:integration:backup")
	}
	engine = mustEnv("TEST_CONTAINER_ENGINE")
	composeFile = mustEnv("TEST_BACKUP_COMPOSE_FILE")
	suDSN = mustEnv("TEST_BACKUP_DATABASE_DSN")
	s3Key, s3Secret = mustEnv("TEST_S3_ACCESS_KEY"), mustEnv("TEST_S3_SECRET_KEY")

	cert, err := tls.LoadX509KeyPair(mustEnv("TEST_NATS_CERT"), mustEnv("TEST_NATS_KEY"))
	Expect(err).NotTo(HaveOccurred())
	caPEM, err := os.ReadFile(mustEnv("TEST_NATS_CA"))
	Expect(err).NotTo(HaveOccurred())
	pool := x509.NewCertPool()
	Expect(pool.AppendCertsFromPEM(caPEM)).To(BeTrue())
	natsTLS = &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: pool, MinVersion: tls.VersionTLS12}
})

// stack runs compose and container commands for one destination.
type stack struct {
	dest      string // "local" or "s3"
	backupDSN string // backup_user DSN as seen from inside the compose network
}

func (s *stack) run(name string, args ...string) (string, int) {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "BACKUP_DEST="+s.dest, "CROSSCODEX_BACKUP_DSN="+s.backupDSN)
	out, err := cmd.CombinedOutput()
	GinkgoWriter.Printf("$ %s %s\n%s\n", name, strings.Join(args, " "), out)
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exitErr):
		return string(out), exitErr.ExitCode()
	}
	Fail(fmt.Sprintf("run %s: %v", name, err))
	return "", -1
}

func (s *stack) compose(args ...string) (string, int) {
	argv := append(slices.Clone(composeCmd[1:]), "-p", project, "-f", composeFile)
	return s.run(composeCmd[0], append(argv, args...)...)
}

func (s *stack) mustCompose(args ...string) string {
	out, code := s.compose(args...)
	ExpectWithOffset(1, code).To(BeZero(), out)
	return out
}

// admin runs `crosscodexd admin backup <args>` in the crosscodexd image.
func (s *stack) admin(args ...string) (string, int) {
	return s.compose(append([]string{"run", "--rm", "admin", "admin", "backup"}, args...)...)
}

func (s *stack) dbRestore(args ...string) (string, int) {
	return s.compose(append([]string{"run", "--rm", "db-restore"}, args...)...)
}

// shell runs script as root with the backup volume at /backup.
func (s *stack) shell(script string) string {
	return s.mustCompose("run", "--rm", "--entrypoint", "sh", "backup-init", "-c", script)
}

func (s *stack) destroyDB() {
	_, _ = s.run(engine, "rm", "-f", dbContainer)
	_, code := s.run(engine, "volume", "rm", project+"_pgdata")
	Expect(code).To(BeZero(), "remove the pgdata volume")
}

// openDB opens the superuser pool for the whole Ordered container. Specs
// destroy and restart the database, so the pool keeps no idle connections:
// each query dials the current server instead of reusing a dead session.
func openDB() *sql.DB {
	db, err := sql.Open("pgx", suDSN)
	Expect(err).NotTo(HaveOccurred())
	db.SetMaxIdleConns(0)
	DeferCleanup(db.Close)
	return db
}

// startDB starts the db service and waits until it accepts connections and
// has left recovery (promoted).
func (s *stack) startDB(db *sql.DB) {
	s.mustCompose("up", "-d", "db")
	Eventually(func(g Gomega) {
		var inRecovery bool
		g.Expect(db.QueryRow("SELECT pg_is_in_recovery()").Scan(&inRecovery)).To(Succeed())
		g.Expect(inRecovery).To(BeFalse())
	}).WithTimeout(3 * time.Minute).WithPolling(2 * time.Second).Should(Succeed())
}

// switchAndWaitArchived closes the current WAL segment, waits until WAL-G
// archived it, and returns its name.
func switchAndWaitArchived(db *sql.DB) string {
	var seg string
	Expect(db.QueryRow("SELECT pg_walfile_name(pg_switch_wal())").Scan(&seg)).To(Succeed())
	Eventually(func(g Gomega) {
		var last sql.NullString
		g.Expect(db.QueryRow("SELECT last_archived_wal FROM pg_stat_archiver").Scan(&last)).To(Succeed())
		g.Expect(last.Valid && last.String >= seg).To(BeTrue(), "last archived %q, want >= %q", last.String, seg)
	}).WithTimeout(2 * time.Minute).WithPolling(time.Second).Should(Succeed())
	return seg
}

func s3Client() *s3.Client {
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(s3Key, s3Secret, "")))
	Expect(err).NotTo(HaveOccurred())
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(s3Endpoint)
		o.UsePathStyle = true
	})
}

func s3Keys(bucket, prefix string) []string {
	out, err := s3Client().ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
	Expect(err).NotTo(HaveOccurred())
	var keys []string
	for _, o := range out.Contents {
		keys = append(keys, aws.ToString(o.Key))
	}
	slices.Sort(keys)
	return keys
}

func objectStore(tenantID string) storage.Provider {
	p, err := storage.NewS3(objectsBucket, tenantID,
		storage.WithEndpoint(s3Endpoint), storage.WithRegion("us-east-1"), storage.WithCredentials(s3Key, s3Secret))
	Expect(err).NotTo(HaveOccurred())
	return p
}

// objectContents returns tenant -> key -> content.
func objectContents() map[string]map[string]string {
	ctx := context.Background()
	out := map[string]map[string]string{}
	for _, t := range tenants {
		p := objectStore(t)
		objs, err := p.List(ctx, "")
		Expect(err).NotTo(HaveOccurred())
		out[t] = map[string]string{}
		for _, o := range objs {
			rc, err := p.Get(ctx, o.Key)
			Expect(err).NotTo(HaveOccurred())
			b, err := io.ReadAll(rc)
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.Close()).To(Succeed())
			out[t][o.Key] = string(b)
		}
		Expect(p.Close()).To(Succeed())
	}
	return out
}

func deleteAllObjects() {
	ctx := context.Background()
	for _, t := range tenants {
		p := objectStore(t)
		objs, err := p.List(ctx, "")
		Expect(err).NotTo(HaveOccurred())
		for _, o := range objs {
			Expect(p.Delete(ctx, o.Key)).To(Succeed())
		}
		Expect(p.Close()).To(Succeed())
	}
}

func connectJetStream() (*nats.Conn, jetstream.JetStream) {
	var nc *nats.Conn
	Eventually(func() error {
		var err error
		nc, err = nats.Connect(natsURL, nats.Secure(natsTLS))
		return err
	}).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed())
	js, err := jetstream.New(nc)
	Expect(err).NotTo(HaveOccurred())
	return nc, js
}

// streamMessages returns stream -> "seq|subject|data" for every message.
func streamMessages(js jetstream.JetStream) map[string][]string {
	ctx := context.Background()
	out := map[string][]string{}
	for _, name := range natsbus.AuditStreamNames() {
		s, err := js.Stream(ctx, name)
		Expect(err).NotTo(HaveOccurred())
		info, err := s.Info(ctx)
		Expect(err).NotTo(HaveOccurred())
		var msgs []string
		for seq := info.State.FirstSeq; seq <= info.State.LastSeq && info.State.Msgs > 0; seq++ {
			m, err := s.GetMsg(ctx, seq)
			Expect(err).NotTo(HaveOccurred())
			msgs = append(msgs, fmt.Sprintf("%d|%s|%s", m.Sequence, m.Subject, m.Data))
		}
		out[name] = msgs
	}
	return out
}

func dbRows(db *sql.DB) []string {
	rows, err := db.Query(`SELECT t.tenant_id || '|' || r.payload FROM backup_it_rows r JOIN tenants t USING (tenant_id) ORDER BY r.id`)
	Expect(err).NotTo(HaveOccurred())
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		Expect(rows.Scan(&v)).To(Succeed())
		out = append(out, v)
	}
	Expect(rows.Err()).NotTo(HaveOccurred())
	return out
}

func destinationSpecs(dest string) func() {
	return func() {
		ctx := context.Background()
		var (
			s        *stack
			db       *sql.DB
			nc       *nats.Conn
			js       jetstream.JetStream
			pointID  string
			walgName string
			seeded   struct {
				rows     []string
				objects  map[string]map[string]string
				messages map[string][]string
			}
		)

		BeforeAll(func() {
			pw, err := dbtest.NewRolePassword()
			Expect(err).NotTo(HaveOccurred())
			s = &stack{dest: dest, backupDSN: (&url.URL{
				Scheme: "postgres", User: url.UserPassword("backup_user", pw), Host: "db:5432", Path: "/crosscodex_test",
				RawQuery: "sslmode=verify-full&sslrootcert=/admin-certs/ca.pem&sslcert=/admin-certs/client.pem&sslkey=/admin-certs/client-key.pem",
			}).String()}
			_, _ = s.compose("down", "-v") // leftovers from an interrupted run
			DeferCleanup(func() { _, _ = s.compose("down", "-v") })

			s.mustCompose("run", "--rm", "backup-init")
			s.mustCompose("up", "-d", "db", "storage", "nats")
			db = openDB()
			s.startDB(db)

			Eventually(func() error {
				for _, b := range []string{objectsBucket, backupBucket} {
					_, err := s3Client().CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(b)})
					if err != nil && !strings.Contains(err.Error(), "BucketAlreadyOwnedByYou") {
						return err
					}
				}
				return nil
			}).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed())

			m, err := dbpkg.NewMigrator(suDSN)
			Expect(err).NotTo(HaveOccurred())
			if err := m.Up(ctx); err != nil && !errors.Is(err, migrate.ErrNoChange) {
				Fail(err.Error())
			}
			Expect(m.Close()).To(Succeed())
			stmt, err := dbtest.AlterRolePasswordSQL("backup_user", pw)
			Expect(err).NotTo(HaveOccurred())
			_, err = db.Exec(stmt)
			Expect(err).NotTo(HaveOccurred())

			// natsbus.New creates the audit streams exactly as the daemon does.
			bus, err := natsbus.New(config.NATSConfig{URL: natsURL, TLS: true, Streams: config.NATSStreamsConfig{
				AuditLLMRetention: 90 * 24 * time.Hour, AuditEventsRetention: 30 * 24 * time.Hour,
			}}, natsbus.WithTLSConfig(natsTLS))
			Expect(err).NotTo(HaveOccurred())
			Expect(bus.Close()).To(Succeed())
			nc, js = connectJetStream()
			DeferCleanup(nc.Close)

			_, err = db.Exec(`CREATE TABLE IF NOT EXISTS backup_it_rows (id serial PRIMARY KEY, tenant_id text NOT NULL REFERENCES tenants(tenant_id), payload text NOT NULL)`)
			Expect(err).NotTo(HaveOccurred())
			for _, t := range tenants {
				_, err = db.Exec(`INSERT INTO tenants (tenant_id, display_name) VALUES ($1, $1) ON CONFLICT DO NOTHING`, t)
				Expect(err).NotTo(HaveOccurred())
				_, err = db.Exec(`INSERT INTO backup_it_rows (tenant_id, payload) VALUES ($1, 'row-' || $1)`, t)
				Expect(err).NotTo(HaveOccurred())
				p := objectStore(t)
				for key, data := range map[string]string{
					"artifacts/a.json": "alpha-" + t, "artifacts/shared.json": "shared", "catalogs/c.json": "gamma-" + t,
				} {
					Expect(p.Put(ctx, key, strings.NewReader(data))).To(Succeed())
				}
				Expect(p.Close()).To(Succeed())
				for _, kind := range []string{"llm", "decisions", "events"} {
					for i := range 3 {
						_, err := js.Publish(ctx, fmt.Sprintf("crosscodex.audit.%s.%s.seed", t, kind), []byte(fmt.Sprintf("%s-%s-%d", t, kind, i)))
						Expect(err).NotTo(HaveOccurred())
					}
				}
			}
			switchAndWaitArchived(db) // archiving works before anything relies on it
			seeded.rows, seeded.objects, seeded.messages = dbRows(db), objectContents(), streamMessages(js)
		})

		// A database that fails recovery exits, and teardown removes it with
		// its log; keep the log for the report.
		AfterEach(func() {
			if CurrentSpecReport().Failed() {
				_, _ = s.run(engine, "logs", "--tail", "60", dbContainer)
			}
		})

		It("captures all three stores in one point", func() {
			out, code := s.admin("run")
			Expect(code).To(BeZero(), out)
			m := pointRe.FindStringSubmatch(out)
			Expect(m).NotTo(BeNil(), out)
			pointID = m[1]
			walgName = walgNameRe.FindString(out)
			Expect(walgName).NotTo(BeEmpty(), out)

			out, code = s.admin("list")
			Expect(code).To(BeZero(), out)
			Expect(out).To(ContainSubstring(pointID))
			out, code = s.admin("verify")
			Expect(code).To(BeZero(), out)
		})

		It("crosscodex-db-restore refuses a non-empty PGDATA and leaves it intact", func() {
			_, _ = s.run(engine, "stop", dbContainer)
			out, code := s.dbRestore(walgName)
			Expect(code).To(Equal(1), out)
			Expect(out).To(ContainSubstring("is not empty"))
			s.startDB(db)
			Expect(dbRows(db)).To(Equal(seeded.rows))
		})

		It("admin restore refuses non-empty targets without writing anything", func() {
			out, code := s.admin("restore", "--point", pointID)
			Expect(code).To(Equal(1), out)
			Expect(out).To(ContainSubstring(`tenant "acme-corp"`))
			Expect(out).To(ContainSubstring("already holds"))
			Expect(objectContents()).To(Equal(seeded.objects))

			deleteAllObjects()
			out, code = s.admin("restore", "--point", pointID)
			Expect(code).To(Equal(1), out)
			Expect(out).To(ContainSubstring(`stream "AUDIT_LLM"`))
			Expect(objectContents()).To(Equal(map[string]map[string]string{"acme-corp": {}, "globex": {}}), "no partial object writes")
			Expect(streamMessages(js)).To(Equal(seeded.messages))
		})

		It("restores every store byte for byte after total data loss", func() {
			for _, name := range natsbus.AuditStreamNames() {
				Expect(js.DeleteStream(ctx, name)).To(Succeed())
			}
			s.destroyDB()

			out, code := s.dbRestore(walgName)
			Expect(code).To(BeZero(), out)
			s.startDB(db)
			out, code = s.admin("restore", "--point", pointID)
			Expect(code).To(BeZero(), out)

			Expect(dbRows(db)).To(Equal(seeded.rows))
			Expect(objectContents()).To(Equal(seeded.objects))
			Expect(streamMessages(js)).To(Equal(seeded.messages), "same messages at the same sequence numbers")
			out, code = s.admin("verify")
			Expect(code).To(BeZero(), out)
		})

		It("restores Postgres to a point in time", func() {
			out, code := s.admin("run")
			Expect(code).To(BeZero(), out)
			base := walgNameRe.FindString(out)

			_, err := db.Exec(`INSERT INTO backup_it_rows (tenant_id, payload) VALUES ('acme-corp', 'A')`)
			Expect(err).NotTo(HaveOccurred())
			time.Sleep(2 * time.Second)
			var target time.Time
			Expect(db.QueryRow("SELECT clock_timestamp()").Scan(&target)).To(Succeed())
			time.Sleep(2 * time.Second)
			_, err = db.Exec(`INSERT INTO backup_it_rows (tenant_id, payload) VALUES ('acme-corp', 'B')`)
			Expect(err).NotTo(HaveOccurred())
			switchAndWaitArchived(db)

			s.destroyDB()
			out, code = s.dbRestore(base, "--target-time", target.UTC().Format(time.RFC3339Nano))
			Expect(code).To(BeZero(), out)
			s.startDB(db)
			rows := dbRows(db)
			Expect(rows).To(ContainElement("acme-corp|A"))
			Expect(rows).NotTo(ContainElement("acme-corp|B"))
		})

		It("verify fails when an archived WAL segment is missing", func() {
			// wal-verify walks back from the current segment and reports gaps
			// among the newest WALG_UPLOAD_CONCURRENCY (default 16) segments
			// as "probably uploading" (WARNING, which must not fail verify).
			// Only an older gap is MISSING_LOST (FAILURE), so archive more
			// segments than that window after the victim.
			const newerSegments = 20
			var victim string
			for i := range newerSegments + 1 {
				_, err := db.Exec(`INSERT INTO backup_it_rows (tenant_id, payload) VALUES ('globex', $1)`, fmt.Sprintf("wal-%d", i))
				Expect(err).NotTo(HaveOccurred())
				if seg := switchAndWaitArchived(db); i == 0 {
					victim = seg
				}
			}
			var archived []string
			if dest == "s3" {
				for _, k := range s3Keys(backupBucket, "postgres/wal_005/") {
					archived = append(archived, strings.TrimPrefix(k, "postgres/wal_005/"))
				}
			} else {
				archived = strings.Fields(s.shell("ls /backup/postgres/wal_005"))
			}
			i := slices.IndexFunc(archived, func(name string) bool { return strings.HasPrefix(name, victim+".") })
			Expect(i).NotTo(Equal(-1), "segment %s is archived: %v", victim, archived)
			if dest == "s3" {
				_, err := s3Client().DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(backupBucket), Key: aws.String("postgres/wal_005/" + archived[i])})
				Expect(err).NotTo(HaveOccurred())
			} else {
				s.shell("rm /backup/postgres/wal_005/" + archived[i])
			}

			out, code := s.admin("verify")
			Expect(code).To(Equal(1), out)
			Expect(out).To(MatchRegexp(`integrity\s+FAILURE`))
			Expect(out).To(ContainSubstring("WAL archive has a gap"))
		})

		It("verify and restore refuse a tampered blob, leaving data unchanged", func() {
			before := objectContents()
			if dest == "s3" {
				keys := s3Keys(backupBucket, "backup/objects/blobs/")
				Expect(keys).NotTo(BeEmpty())
				_, err := s3Client().PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(backupBucket), Key: aws.String(keys[0]), Body: strings.NewReader("tampered")})
				Expect(err).NotTo(HaveOccurred())
			} else {
				s.shell(`f=$(ls /backup/backup/objects/blobs | head -n1) && printf x >> "/backup/backup/objects/blobs/$f"`)
			}

			out, code := s.admin("verify", "--point", pointID)
			Expect(code).To(Equal(1), out)
			Expect(out).To(ContainSubstring("sha256 mismatch"))
			out, code = s.admin("restore", "--point", pointID)
			Expect(code).To(Equal(1), out)
			Expect(out).To(ContainSubstring("failed verification"))
			Expect(objectContents()).To(Equal(before))
		})
	}
}

var _ = Describe("backup with a local destination", Ordered, destinationSpecs("local"))
var _ = Describe("backup with an S3 destination", Ordered, destinationSpecs("s3"))
