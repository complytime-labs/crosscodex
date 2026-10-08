//go:build !integration

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/backup"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
)

// captureStderr swaps os.Stderr for a temp file for the duration of fn, the
// same approach runWithEnv in main_bdd_test.go uses, and returns fn's result
// plus everything written to stderr.
func captureStderr(fn func() int) (int, string) {
	stderrFile, err := os.CreateTemp(GinkgoT().TempDir(), "stderr")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(stderrFile.Close)
	origStderr := os.Stderr
	os.Stderr = stderrFile
	DeferCleanup(func() { os.Stderr = origStderr })

	code := fn()

	os.Stderr = origStderr
	data, err := os.ReadFile(stderrFile.Name())
	Expect(err).NotTo(HaveOccurred())
	return code, string(data)
}

func cmdSampleManifest() *backup.Manifest {
	t0 := time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC)
	stats := func(off time.Duration, n int64) backup.CaptureStats {
		return backup.CaptureStats{Window: backup.Window{Start: t0.Add(off), End: t0.Add(off + 5*time.Second)}, Bytes: n, ThroughputBytesPerSec: float64(n) / 5}
	}
	return &backup.Manifest{
		SchemaVersion: backup.SchemaVersion, ID: "20261007T010000Z-0a1b2c3d", StartedAt: t0, FinishedAt: t0.Add(20 * time.Second),
		Postgres: backup.PostgresCapture{BackupName: "base_000000010000000000000004", Timeline: 1, CaptureStats: stats(0, 4096)},
		Objects: backup.ObjectsCapture{Tenants: map[string][]backup.ObjectEntry{
			"acme-corp": {{Key: "a.json", SHA256: strings.Repeat("a", 64), Size: 5}},
			"globex":    {{Key: "b.json", SHA256: strings.Repeat("b", 64), Size: 7}},
		}, CaptureStats: stats(5*time.Second, 12)},
		NATS: backup.NATSCapture{Streams: []backup.StreamCapture{{
			Stream: "AUDIT_EVENTS",
			Files: []backup.StreamFile{
				{Name: "backup.json", SHA256: strings.Repeat("c", 64), Size: 10},
				{Name: "stream.arc.s2", SHA256: strings.Repeat("d", 64), Size: 90},
			},
			State: backup.StreamState{FirstSeq: 1, LastSeq: 42, Msgs: 42},
		}}, CaptureStats: stats(10*time.Second, 100)},
	}
}

var _ = Describe("runAdmin backup arg parsing", func() {
	DescribeTable("returns 2 for usage errors",
		func(args ...string) {
			Expect(runAdmin(args)).To(Equal(2))
		},
		Entry("backup without a command", "backup"),
		Entry("unknown backup command", "backup", "bogus"),
		Entry("run with an argument", "backup", "run", "extra"),
		Entry("run with an unknown flag", "backup", "run", "--nope"),
		Entry("list with an argument", "backup", "list", "x"),
		Entry("verify with a malformed --point", "backup", "verify", "--point", "../x"),
		Entry("restore without --point", "backup", "restore"),
		Entry("restore with an empty --point", "backup", "restore", "--point", ""),
		Entry("restore with a malformed --point", "backup", "restore", "--point", "nope"),
		Entry("restore with an extra argument", "backup", "restore", "--point", "20261007T010000Z-0a1b2c3d", "x"),
	)

	DescribeTable("returns 0 for -h",
		func(args ...string) {
			Expect(runAdmin(args)).To(Equal(0))
		},
		Entry("run -h", "backup", "run", "-h"),
		Entry("list -h", "backup", "list", "-h"),
		Entry("verify -h", "backup", "verify", "-h"),
		Entry("restore -h", "backup", "restore", "-h"),
	)
})

var _ = Describe("runAdmin backup commands", func() {
	var (
		origRun     func(context.Context, *config.Config) (*backup.Manifest, error)
		origList    func(context.Context, *config.Config) (backup.ListResult, error)
		origVerify  func(context.Context, *config.Config, backup.VerifyOptions) (backup.VerifyReport, error)
		origRestore func(context.Context, *config.Config, string) error
		gotOpts     backup.VerifyOptions
		gotPoint    string
		fnErr       error
		report      backup.VerifyReport
	)

	BeforeEach(func() {
		origRun, origList, origVerify, origRestore = runBackupRunFn, runBackupListFn, runBackupVerifyFn, runBackupRestoreFn
		gotOpts, gotPoint, fnErr, report = backup.VerifyOptions{}, "", nil, backup.VerifyReport{}
		runBackupRunFn = func(context.Context, *config.Config) (*backup.Manifest, error) {
			if fnErr != nil {
				return nil, fnErr
			}
			return cmdSampleManifest(), nil
		}
		runBackupListFn = func(context.Context, *config.Config) (backup.ListResult, error) {
			return backup.ListResult{}, fnErr
		}
		runBackupVerifyFn = func(_ context.Context, _ *config.Config, o backup.VerifyOptions) (backup.VerifyReport, error) {
			gotOpts = o
			return report, fnErr
		}
		runBackupRestoreFn = func(_ context.Context, _ *config.Config, id string) error {
			gotPoint = id
			return fnErr
		}
		// An empty XDG config dir makes the loader fall back to built-in defaults.
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
	})

	AfterEach(func() {
		runBackupRunFn, runBackupListFn, runBackupVerifyFn, runBackupRestoreFn = origRun, origList, origVerify, origRestore
	})

	DescribeTable("exits 0 on success and 1 on failure",
		func(args []string) {
			Expect(runAdmin(args)).To(Equal(0))
			fnErr = errors.New("boom")
			Expect(runAdmin(args)).To(Equal(1))
		},
		Entry("run", []string{"backup", "run"}),
		Entry("list", []string{"backup", "list"}),
		Entry("restore", []string{"backup", "restore", "--point", "20261007T010000Z-0a1b2c3d"}),
	)

	It("verify passes --point through and exits 1 when the report is not OK", func() {
		report = backup.VerifyReport{Staleness: []backup.StoreAge{{Store: "postgres", Stale: true}}}
		Expect(runAdmin([]string{"backup", "verify", "--point", "20261007T010000Z-0a1b2c3d"})).To(Equal(1))
		Expect(gotOpts.PointID).To(Equal("20261007T010000Z-0a1b2c3d"))
	})

	It("verify exits 0 for an OK report and defaults to every point", func() {
		Expect(runAdmin([]string{"backup", "verify"})).To(Equal(0))
		Expect(gotOpts).To(Equal(backup.VerifyOptions{}))
	})

	It("restore passes --point through", func() {
		Expect(runAdmin([]string{"backup", "restore", "--point", "20261007T010000Z-0a1b2c3d"})).To(Equal(0))
		Expect(gotPoint).To(Equal("20261007T010000Z-0a1b2c3d"))
	})
})

var _ = Describe("runAdmin backup verify failure messages", func() {
	var origVerify func(context.Context, *config.Config, backup.VerifyOptions) (backup.VerifyReport, error)

	BeforeEach(func() {
		origVerify = runBackupVerifyFn
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
	})

	AfterEach(func() {
		runBackupVerifyFn = origVerify
	})

	It("reports stale stores only", func() {
		runBackupVerifyFn = func(context.Context, *config.Config, backup.VerifyOptions) (backup.VerifyReport, error) {
			return backup.VerifyReport{Staleness: []backup.StoreAge{
				{Store: "postgres", Stale: true},
				{Store: "objects", Stale: false},
				{Store: "nats", Stale: true},
			}}, nil
		}
		code, stderr := captureStderr(func() int { return runAdmin([]string{"backup", "verify"}) })
		Expect(code).To(Equal(1))
		Expect(stderr).To(ContainSubstring("admin backup verify: FAILED: stale stores: postgres, nats (staleness is repository-wide: it uses the newest point, not only --point)"))
		Expect(stderr).NotTo(ContainSubstring("integrity problems"))
		Expect(stderr).NotTo(ContainSubstring("WAL archive"))
	})

	It("reports point issues only", func() {
		runBackupVerifyFn = func(context.Context, *config.Config, backup.VerifyOptions) (backup.VerifyReport, error) {
			return backup.VerifyReport{Points: []backup.PointReport{
				{ID: "20261007T010000Z-0a1b2c3d", Issues: []string{"blob x sha256 mismatch"}},
				{ID: "20261008T000000Z-ffffffff"},
			}}, nil
		}
		code, stderr := captureStderr(func() int { return runAdmin([]string{"backup", "verify"}) })
		Expect(code).To(Equal(1))
		Expect(stderr).To(ContainSubstring("admin backup verify: FAILED: integrity problems in 1 point(s) (see report)"))
		Expect(stderr).NotTo(ContainSubstring("stale stores"))
		Expect(stderr).NotTo(ContainSubstring("WAL archive"))
	})

	It("reports a WAL gap only", func() {
		runBackupVerifyFn = func(context.Context, *config.Config, backup.VerifyOptions) (backup.VerifyReport, error) {
			return backup.VerifyReport{WAL: []backup.WALCheck{{Name: "integrity", Status: "FAILURE"}}}, nil
		}
		code, stderr := captureStderr(func() int { return runAdmin([]string{"backup", "verify"}) })
		Expect(code).To(Equal(1))
		Expect(stderr).To(ContainSubstring("admin backup verify: FAILED: WAL archive has a gap (see WAL CHECK)"))
		Expect(stderr).NotTo(ContainSubstring("stale stores"))
		Expect(stderr).NotTo(ContainSubstring("integrity problems"))
	})
})

var _ = Describe("backup wiring", func() {
	ctx := context.Background()

	loadDefaults := func() *config.Config {
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		cfg, err := config.NewLoader().Load(ctx)
		Expect(err).NotTo(HaveOccurred())
		return cfg
	}

	It("fails closed, with the fix, when backup is disabled", func() {
		_, err := daemonBackupList(ctx, loadDefaults())
		Expect(err).To(MatchError(ContainSubstring("backup is disabled")))
		Expect(err.Error()).To(ContainSubstring("backup.destination.backend"))
	})

	It("lists an empty local repository", func() {
		cfg := loadDefaults()
		cfg.Backup.Destination = config.BackupDestinationConfig{Backend: "local", Local: config.BackupLocalConfig{Path: GinkgoT().TempDir()}}
		res, err := daemonBackupList(ctx, cfg)
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(backup.ListResult{}))
	})

	It("refuses JetStream backup in embedded NATS mode", func() {
		cfg := loadDefaults()
		cfg.Backup.Destination = config.BackupDestinationConfig{Backend: "local", Local: config.BackupLocalConfig{Path: GinkgoT().TempDir()}}
		cfg.NATS.URL = ""
		_, closeFn, err := openBackup(ctx, cfg, backupNeeds{nats: true})
		Expect(closeFn).To(BeNil())
		Expect(err).To(MatchError(natsbus.ErrExternalURLRequired))
	})
})

var _ = Describe("backup reports", func() {
	It("run report names the point and every store", func() {
		var buf bytes.Buffer
		writeBackupRunReport(&buf, cmdSampleManifest())
		out := buf.String()
		Expect(out).To(ContainSubstring("backup point 20261007T010000Z-0a1b2c3d committed"))
		Expect(out).To(ContainSubstring("base_000000010000000000000004"))
		Expect(out).To(ContainSubstring("2 tenants, 2 objects"))
		Expect(out).To(ContainSubstring("1 streams, 42 messages"))
		Expect(out).To(ContainSubstring("2026-10-07T01:00:00Z .. 2026-10-07T01:00:05Z"))
	})

	It("list shows complete points per store and incomplete points separately", func() {
		var buf bytes.Buffer
		writeBackupList(&buf, backup.ListResult{Complete: []*backup.Manifest{cmdSampleManifest()}, Incomplete: []string{"20261008T000000Z-ffffffff"}})
		out := buf.String()
		Expect(out).To(ContainSubstring("20261007T010000Z-0a1b2c3d"))
		Expect(out).To(MatchRegexp(`objects\s+2026-10-07T01:00:05Z`))
		Expect(out).To(ContainSubstring("incomplete"))
		Expect(out).To(ContainSubstring("20261008T000000Z-ffffffff"))
	})

	It("verify report shows issues, WAL checks, staleness and the restore estimate", func() {
		var buf bytes.Buffer
		writeVerifyReport(&buf, backup.VerifyReport{
			Points:    []backup.PointReport{{ID: "20261007T010000Z-0a1b2c3d", Issues: []string{"blob x sha256 mismatch"}, EstimatedRestore: 90 * time.Second}},
			WAL:       []backup.WALCheck{{Name: "integrity", Status: "FAILURE"}},
			Staleness: []backup.StoreAge{{Store: "nats", NewestPoint: "20261007T010000Z-0a1b2c3d", Age: 30 * time.Hour, MaxAge: 26 * time.Hour, Stale: true}},
		})
		out := buf.String()
		Expect(out).To(ContainSubstring("FAILED"))
		Expect(out).To(ContainSubstring("blob x sha256 mismatch"))
		Expect(out).To(ContainSubstring("1m30s"))
		Expect(out).To(MatchRegexp(`integrity\s+FAILURE`))
		Expect(out).To(MatchRegexp(`nats\s+20261007T010000Z-0a1b2c3d\s+30h0m0s\s+26h0m0s\s+STALE`))
	})

	It("verify report says when there are no complete points", func() {
		var buf bytes.Buffer
		writeVerifyReport(&buf, backup.VerifyReport{Staleness: []backup.StoreAge{{Store: "postgres", Stale: true}}})
		Expect(buf.String()).To(ContainSubstring("no complete backup points"))
		Expect(buf.String()).To(MatchRegexp(`postgres\s+\(none\)`))
	})
})
