package backup_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/backup"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("WALG", func() {
	ctx := context.Background()
	local := config.BackupDestinationConfig{Backend: "local", Local: config.BackupLocalConfig{Path: "/srv/b"}}
	const dsn = "postgres://backup_user:pw-123@db.internal:5433/crosscodex?sslmode=verify-full&sslrootcert=/certs/ca.pem"

	type call struct{ env, argv []string }
	var calls []call
	recorder := func(out string, err error) backup.CommandRunner {
		return func(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
			calls = append(calls, call{env: env, argv: append([]string{name}, args...)})
			return []byte(out), err
		}
	}
	BeforeEach(func() { calls = nil })

	It("passes connection and destination settings in the environment, never in argv", func() {
		w, err := backup.NewWALG(dsn, local, recorder("", nil))
		Expect(err).NotTo(HaveOccurred())
		Expect(w.BackupPush(ctx)).To(Succeed())
		Expect(calls[0].argv).To(Equal([]string{"wal-g", "backup-push"}), "no PGDATA argument: remote BASE_BACKUP mode")
		Expect(calls[0].env).To(ContainElements(
			"PGHOST=db.internal", "PGPORT=5433", "PGUSER=backup_user", "PGPASSWORD=pw-123",
			"PGDATABASE=crosscodex", "PGSSLMODE=verify-full", "PGSSLROOTCERT=/certs/ca.pem",
			"WALG_FILE_PREFIX=/srv/b/postgres",
		))
		Expect(calls[0].argv).NotTo(ContainElement(ContainSubstring("pw-123")))
	})

	It("maps an S3 destination to WAL-G's S3 settings", func() {
		s3 := config.BackupDestinationConfig{Backend: "s3", S3: config.BackupS3Config{
			Bucket: "bk", Region: "eu-west-1", Endpoint: "http://storage:9000", StorageClass: "STANDARD_IA",
		}}
		w, err := backup.NewWALG(dsn, s3, recorder("", nil))
		Expect(err).NotTo(HaveOccurred())
		Expect(w.BackupPush(ctx)).To(Succeed())
		Expect(calls[0].env).To(ContainElements(
			"WALG_S3_PREFIX=s3://bk/postgres", "AWS_REGION=eu-west-1", "AWS_ENDPOINT=http://storage:9000",
			"AWS_S3_FORCE_PATH_STYLE=true", "WALG_S3_STORAGE_CLASS=STANDARD_IA",
		))
	})

	It("drops inherited WAL-G, libpq and endpoint settings so config is the only source", func() {
		GinkgoT().Setenv("WALG_S3_PREFIX", "s3://elsewhere")
		GinkgoT().Setenv("PGPASSFILE", "/tmp/pgpass")
		GinkgoT().Setenv("AWS_ENDPOINT", "http://elsewhere")
		GinkgoT().Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLE")
		w, err := backup.NewWALG(dsn, local, recorder("", nil))
		Expect(err).NotTo(HaveOccurred())
		Expect(w.BackupPush(ctx)).To(Succeed())
		env := calls[0].env
		Expect(env).NotTo(ContainElement(HavePrefix("WALG_S3_PREFIX=")))
		Expect(env).NotTo(ContainElement(HavePrefix("PGPASSFILE=")))
		Expect(env).NotTo(ContainElement(HavePrefix("AWS_ENDPOINT=")))
		Expect(env).To(ContainElement("AWS_ACCESS_KEY_ID=AKIAEXAMPLE"), "the AWS credential chain keeps working")
	})

	DescribeTable("drops every inherited variable not on the allowlist, however WAL-G might read it",
		func(name string) {
			GinkgoT().Setenv(name, "leaked-value")
			w, err := backup.NewWALG(dsn, local, recorder("", nil))
			Expect(err).NotTo(HaveOccurred())
			Expect(w.BackupPush(ctx)).To(Succeed())
			// Check the exact leaked pair, not just the prefix: PGSSLROOTCERT
			// is also set legitimately from the DSN's sslrootcert parameter,
			// so a bare prefix check would misfire on that unrelated value.
			Expect(calls[0].env).NotTo(ContainElement(name + "=leaked-value"))
		},
		Entry("WALE_S3_PREFIX: WAL-G's WALE_ fallback for every WALG_ setting", "WALE_S3_PREFIX"),
		Entry("S3_STORAGE_CLASS: unprefixed S3 settings win over WALG_ ones", "S3_STORAGE_CLASS"),
		Entry("S3_ENDPOINT_SOURCE", "S3_ENDPOINT_SOURCE"),
		Entry("GPG_KEY_ID", "GPG_KEY_ID"),
		Entry("PGSSLROOTCERT", "PGSSLROOTCERT"),
		Entry("WALG_S3_PREFIX", "WALG_S3_PREFIX"),
		Entry("AWS_ENDPOINT", "AWS_ENDPOINT"),
	)

	It("keeps PATH and the AWS credential chain from the inherited environment", func() {
		GinkgoT().Setenv("AWS_ACCESS_KEY_ID", "AKIAEXAMPLE")
		w, err := backup.NewWALG(dsn, local, recorder("", nil))
		Expect(err).NotTo(HaveOccurred())
		Expect(w.BackupPush(ctx)).To(Succeed())
		Expect(calls[0].env).To(ContainElement(HavePrefix("PATH=")))
		Expect(calls[0].env).To(ContainElement("AWS_ACCESS_KEY_ID=AKIAEXAMPLE"))
	})

	It("keeps the inherited AWS_REGION only when the config sets none, and never duplicates it", func() {
		GinkgoT().Setenv("AWS_REGION", "us-east-1")
		s3 := config.BackupDestinationConfig{Backend: "s3", S3: config.BackupS3Config{Bucket: "bk", Region: "eu-west-1"}}
		w, err := backup.NewWALG(dsn, s3, recorder("", nil))
		Expect(err).NotTo(HaveOccurred())
		Expect(w.BackupPush(ctx)).To(Succeed())
		var regions []string
		for _, kv := range calls[0].env {
			if strings.HasPrefix(kv, "AWS_REGION=") {
				regions = append(regions, kv)
			}
		}
		Expect(regions).To(Equal([]string{"AWS_REGION=eu-west-1"}), "config's region wins and appears exactly once")
	})

	DescribeTable("rejects a DSN it cannot map, without echoing it or its password",
		func(bad, leaked string) {
			_, err := backup.NewWALG(bad, local, recorder("", nil))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).NotTo(ContainSubstring(leaked))
		},
		Entry("keyword=value form", "host=db password=pw-123", "pw-123"),
		Entry("other scheme", "mysql://u:pw-123@db/x", "pw-123"),
		Entry("unsupported parameter", "postgres://u:pw-123@db/x?options=-csearch_path%3Devil", "pw-123"),
		Entry("multiple hosts", "postgres://u:pw-123@db1,db2/x", "pw-123"),
		Entry("malformed query encoding (a bad sslmode must not be silently dropped)", "postgres://u:pw-123@db/x?sslmode=%zz", "pw-123"),
		Entry("duplicate parameter", "postgres://u:pw-123@db/x?sslmode=verify-full&sslmode=disable", "pw-123"),
		Entry("no user", "postgres://db/x", "pw-123"),
		Entry("url.Parse error from a space in the password", "postgres://u:p w@h/d", "p w"),
	)

	It("surfaces WAL-G's error", func() {
		w, err := backup.NewWALG(dsn, local, recorder("", errors.New("wal-g backup-push: exit status 1: connection refused")))
		Expect(err).NotTo(HaveOccurred())
		Expect(w.BackupPush(ctx)).To(MatchError(ContainSubstring("connection refused")))
	})

	It("parses backup-list output", func() {
		w, err := backup.NewWALG(dsn, local, recorder(`[{"backup_name":"base_000000010000000000000004","wal_file_name":"000000010000000000000004","time":"2026-10-07T01:00:00Z","start_time":"2026-10-07T01:00:00Z","finish_time":"2026-10-07T01:00:05Z","start_lsn":67108904,"finish_lsn":67109120,"compressed_size":4096,"is_permanent":false}]`, nil))
		Expect(err).NotTo(HaveOccurred())
		list, err := w.Backups(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(calls[0].argv).To(Equal([]string{"wal-g", "backup-list", "--json", "--detail"}))
		Expect(list).To(HaveLen(1))
		Expect(list[0].Name).To(Equal("base_000000010000000000000004"))
		Expect(list[0].StartLSN).To(Equal(uint64(67108904)))
		Expect(list[0].CompressedSize).To(Equal(int64(4096)))
		Expect(list[0].Timeline()).To(Equal(uint32(1)))
	})

	It("rejects a malformed backup-list entry", func() {
		w, err := backup.NewWALG(dsn, local, recorder(`[{"backup_name":"base_../../x","wal_file_name":"000000010000000000000004"}]`, nil))
		Expect(err).NotTo(HaveOccurred())
		_, err = w.Backups(ctx)
		Expect(err).To(MatchError(ContainSubstring("malformed")))
	})

	DescribeTable("accepts an empty backup-list result (there is no base backup yet)",
		func(out string) {
			w, err := backup.NewWALG(dsn, local, recorder(out, nil))
			Expect(err).NotTo(HaveOccurred())
			list, err := w.Backups(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(list).To(BeEmpty())
		},
		Entry("empty JSON array", `[]`),
		Entry("JSON null", `null`),
	)

	DescribeTable("rejects backup-list output that isn't JSON at all",
		func(out string) {
			w, err := backup.NewWALG(dsn, local, recorder(out, nil))
			Expect(err).NotTo(HaveOccurred())
			_, err = w.Backups(ctx)
			Expect(err).To(MatchError(ContainSubstring("printed no JSON")))
		},
		Entry("empty output", ``),
		Entry("whitespace-only output", "   \n"),
	)

	DescribeTable("parses wal-verify JSON (wal-verify exits 0 whatever it finds)",
		func(out string, want []backup.WALCheck, wantErr string) {
			w, err := backup.NewWALG(dsn, local, recorder(out, nil))
			Expect(err).NotTo(HaveOccurred())
			got, err := w.VerifyWAL(ctx)
			if wantErr != "" {
				Expect(err).To(MatchError(ContainSubstring(wantErr)))
				return
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(calls[0].argv).To(Equal([]string{"wal-g", "wal-verify", "integrity", "timeline", "--json"}))
			Expect(got).To(Equal(want))
		},
		Entry("all OK", `{"integrity":{"status":"OK","details":[]},"timeline":{"status":"OK","details":{}}}`,
			[]backup.WALCheck{{Name: "integrity", Status: "OK"}, {Name: "timeline", Status: "OK"}}, ""),
		Entry("lost segment", `{"integrity":{"status":"FAILURE"},"timeline":{"status":"OK"}}`,
			[]backup.WALCheck{{Name: "integrity", Status: "FAILURE"}, {Name: "timeline", Status: "OK"}}, ""),
		Entry("segments delayed", `{"integrity":{"status":"WARNING"},"timeline":{"status":"OK"}}`,
			[]backup.WALCheck{{Name: "integrity", Status: "WARNING"}, {Name: "timeline", Status: "OK"}}, ""),
		Entry("missing check", `{"integrity":{"status":"OK"}}`, nil, `lacks the "timeline" check`),
		Entry("unknown status", `{"integrity":{"status":"MAYBE"},"timeline":{"status":"OK"}}`, nil, "unknown status"),
		Entry("not JSON", `ERROR`, nil, "parse wal-g wal-verify output"),
	)
})

var _ = Describe("WALGBackup", func() {
	Describe("Timeline", func() {
		DescribeTable("parses the first 8 hex digits of wal_file_name",
			func(walFileName string, want uint32) {
				b := backup.WALGBackup{Name: "base_000000010000000000000004", WALFileName: walFileName}
				tl, err := b.Timeline()
				Expect(err).NotTo(HaveOccurred())
				Expect(tl).To(Equal(want))
			},
			Entry("timeline 1", "000000010000000000000004", uint32(1)),
			Entry("timeline 10", "0000000A0000000000000004", uint32(10)),
		)

		DescribeTable("rejects a wal_file_name it cannot read a timeline from",
			func(walFileName string) {
				b := backup.WALGBackup{Name: "base_000000010000000000000004", WALFileName: walFileName}
				_, err := b.Timeline()
				Expect(err).To(HaveOccurred())
			},
			Entry("too short to hold a 24-digit WAL segment name", "0004"),
			Entry("timeline 0 is not a valid PostgreSQL timeline", "000000000000000000000004"),
		)
	})
})

var _ = Describe("ExecRunner", func() {
	ctx := context.Background()

	It("runs with exactly the given environment and returns stdout", func() {
		GinkgoT().Setenv("CROSSCODEX_LEAK", "x")
		out, err := backup.ExecRunner(ctx, []string{"PATH=" + os.Getenv("PATH"), "FOO=bar"},
			"sh", "-c", `printf '%s|%s' "$FOO" "$CROSSCODEX_LEAK"`)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).To(Equal("bar|"))
	})

	It("returns the exit status and stderr on failure", func() {
		_, err := backup.ExecRunner(ctx, []string{"PATH=" + os.Getenv("PATH")}, "sh", "-c", "echo boom >&2; exit 3")
		Expect(err).To(MatchError(ContainSubstring("exit status 3")))
		Expect(err.Error()).To(ContainSubstring("boom"))
	})

	It("terminates the command and reports context cancellation when its context is canceled", func() {
		cancelCtx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			_, err := backup.ExecRunner(cancelCtx, []string{"PATH=" + os.Getenv("PATH")}, "sleep", "30")
			done <- err
		}()
		time.Sleep(100 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			Expect(errors.Is(err, context.Canceled)).To(BeTrue(), "got: %v", err)
		case <-time.After(10 * time.Second):
			Fail("ExecRunner did not return after its context was canceled")
		}
	})
})
