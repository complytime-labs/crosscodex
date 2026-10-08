package backup_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/backup"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
)

// fakeClock advances one second per call, so capture windows have width.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { c.t = c.t.Add(time.Second); return c.t }

// fakeWALG emulates the three wal-g commands the service uses. pushAdds
// controls how many new base backups backup-push adds; newHarness sets it
// to 1 (the normal case), and specs that need findNewBackup's "not exactly
// one new backup" error set it to 0 or 2.
type fakeWALG struct {
	backups   []backup.WALGBackup
	pushErr   error
	walStatus string
	pushAdds  int
}

func (f *fakeWALG) run(_ context.Context, _ []string, _ string, args ...string) ([]byte, error) {
	switch strings.Join(args, " ") {
	case "backup-list --json --detail":
		return json.Marshal(f.backups)
	case "backup-push":
		if f.pushErr != nil {
			return nil, f.pushErr
		}
		for i := 0; i < f.pushAdds; i++ {
			n := len(f.backups) + 2
			seg := fmt.Sprintf("00000001%016X", n)
			f.backups = append(f.backups, backup.WALGBackup{
				Name: "base_" + seg, WALFileName: seg,
				StartLSN: uint64(n) << 24, FinishLSN: uint64(n)<<24 + 0x100, CompressedSize: 4096,
			})
		}
		return nil, nil
	case "wal-verify integrity timeline --json":
		return []byte(fmt.Sprintf(`{"integrity":{"status":%q},"timeline":{"status":"OK"}}`, f.walStatus)), nil
	}
	return nil, fmt.Errorf("unexpected wal-g call %q", args)
}

type fakeTenants []string

func (f fakeTenants) TenantIDs(context.Context) ([]string, error) { return f, nil }

// erroringTenants fails TenantIDs, so Run's objects step can be made to
// fail without touching the object store.
type erroringTenants struct{ err error }

func (e erroringTenants) TenantIDs(context.Context) ([]string, error) { return nil, e.err }

type fakeLock struct {
	held               bool
	releaseErr         error
	acquired, released int
}

func (l *fakeLock) TryAcquire(context.Context) (func(context.Context) error, error) {
	if l.held {
		return nil, fmt.Errorf("advisory lock %q: %w", db.BackupRetentionLockName, db.ErrLockHeld)
	}
	l.acquired++
	return func(context.Context) error { l.released++; return l.releaseErr }, nil
}

type harness struct {
	svc      *backup.Service
	repo     *backup.Repository
	repoRoot string
	walg     *fakeWALG
	src      *objectStores
	streams  *fakeStreamStore
	lock     *fakeLock
	clock    *fakeClock
	tp       *telemetrytest.TestProvider
	deps     backup.Deps
}

func newHarness() *harness {
	h := &harness{
		walg: &fakeWALG{walStatus: "OK", pushAdds: 1}, src: newObjectStores(), streams: newFakeStreamStore(),
		lock: &fakeLock{}, clock: &fakeClock{t: sampleTime},
	}
	h.repo, h.repoRoot = newLocalRepo()
	h.src.put("acme-corp", "artifacts/a.json", "alpha")
	h.src.put("globex", "artifacts/a.json", "alpha")
	h.src.put("globex", "catalogs/c.json", "gamma")
	for _, n := range natsbus.AuditStreamNames() {
		h.streams.streams[n] = fakeStream{state: backup.StreamState{FirstSeq: 1, LastSeq: 2, Msgs: 2}, data: "data-" + n}
	}
	walg, err := backup.NewWALG("postgres://backup_user@db/crosscodex",
		config.BackupDestinationConfig{Backend: "local", Local: config.BackupLocalConfig{Path: h.repoRoot}}, h.walg.run)
	Expect(err).NotTo(HaveOccurred())
	h.tp, err = telemetrytest.NewTestProvider()
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(h.tp.Shutdown, context.Background())
	h.deps = backup.Deps{
		Repo: h.repo, WALG: walg, Tenants: fakeTenants{"acme-corp", "globex"},
		Objects: h.src.open, Streams: h.streams, StreamNames: natsbus.AuditStreamNames(), Lock: h.lock,
		MaxAge:  backup.MaxAge{Postgres: time.Hour, Objects: time.Hour, NATS: time.Hour},
		TempDir: GinkgoT().TempDir(), Now: h.clock.Now, Rand: bytes.NewReader(bytes.Repeat([]byte{0xab}, 64)),
		Tracer: h.tp.TracerProvider().Tracer("test"), Meter: h.tp.MeterProvider().Meter("test"),
	}
	h.svc, err = backup.New(h.deps)
	Expect(err).NotTo(HaveOccurred())
	return h
}

// restoreTarget returns a service sharing h's repository and WAL-G but
// restoring into fresh, empty object and stream stores.
func (h *harness) restoreTarget() (*backup.Service, *objectStores, *fakeStreamStore) {
	dst, dstStreams := newObjectStores(), newFakeStreamStore()
	deps := h.deps
	deps.Objects, deps.Streams = dst.open, dstStreams
	svc, err := backup.New(deps)
	Expect(err).NotTo(HaveOccurred())
	return svc, dst, dstStreams
}

func (h *harness) tamperFirstBlob() {
	dir := filepath.Join(h.repoRoot, "backup", "objects", "blobs")
	entries, err := os.ReadDir(dir)
	Expect(err).NotTo(HaveOccurred())
	Expect(entries).NotTo(BeEmpty())
	Expect(os.WriteFile(filepath.Join(dir, entries[0].Name()), []byte("tampered"), 0o600)).To(Succeed())
}

func spanNames(tp *telemetrytest.TestProvider) []string {
	var names []string
	for _, s := range tp.GetSpans() {
		names = append(names, s.Name())
	}
	return names
}

func metricNames(tp *telemetrytest.TestProvider) []string {
	var names []string
	for _, sm := range tp.GetMetrics().ScopeMetrics {
		for _, m := range sm.Metrics {
			names = append(names, m.Name)
		}
	}
	return names
}

// putErrProvider wraps a local object store and makes every Put fail with
// err, so a restore write failure can be injected without a dedicated fake
// storage.Provider.
type putErrProvider struct {
	storage.Provider
	err error
}

func (p *putErrProvider) Put(context.Context, string, io.Reader) error { return p.err }

// failingPutOpener opens tenant stores under root that list and open
// normally (so preflight sees them empty) but fail every write, driving
// Restore into a mid-write failure.
func failingPutOpener(root string) backup.ObjectStoreOpener {
	return func(tenantID string) (storage.Provider, error) {
		p, err := storage.NewLocal(root, tenantID)
		if err != nil {
			return nil, err
		}
		return &putErrProvider{Provider: p, err: errors.New("disk full")}, nil
	}
}

var _ = Describe("Service", func() {
	ctx := context.Background()
	var h *harness
	BeforeEach(func() { h = newHarness() })

	Describe("Run", func() {
		It("commits one point covering all three stores, holding the lock throughout", func() {
			m, err := h.svc.Run(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(m.Postgres.BackupName).To(Equal("base_000000010000000000000002"))
			Expect(m.Postgres.Timeline).To(Equal(uint32(1)))
			Expect(m.Objects.Tenants).To(HaveLen(2))
			Expect(m.Objects.Bytes).To(Equal(int64(len("alpha")*2 + len("gamma"))))
			Expect(m.NATS.Streams).To(HaveLen(3))
			Expect(m.Postgres.ThroughputBytesPerSec).To(BeNumerically(">", 0))

			pts, err := h.repo.Points(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(pts.Complete).To(Equal([]string{m.ID}))
			Expect(pts.Incomplete).To(BeEmpty())
			Expect(blobFiles(h.repoRoot)).To(HaveLen(2), "alpha is stored once")
			Expect([]int{h.lock.acquired, h.lock.released}).To(Equal([]int{1, 1}))
		})

		It("refuses to start while a retention scan holds the lock, leaving no trace", func() {
			h.lock.held = true
			_, err := h.svc.Run(ctx)
			Expect(err).To(MatchError(db.ErrLockHeld))
			Expect(err.Error()).To(ContainSubstring("retry"))
			pts, err := h.repo.Points(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(pts).To(Equal(backup.PointList{}))
			Expect(h.walg.backups).To(BeEmpty(), "no base backup taken")
		})

		It("leaves an incomplete point, no manifest, and a released lock when a step fails", func() {
			h.walg.pushErr = errors.New("wal-g backup-push: exit status 1: connection refused")
			_, err := h.svc.Run(ctx)
			Expect(err).To(MatchError(ContainSubstring("postgres")))
			Expect(err.Error()).To(ContainSubstring("connection refused"))
			pts, err := h.repo.Points(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(pts.Complete).To(BeEmpty())
			Expect(pts.Incomplete).To(HaveLen(1))
			Expect(h.lock.released).To(Equal(1))
		})

		It("records spans per command and store step, and metrics", func() {
			_, err := h.svc.Run(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(spanNames(h.tp)).To(ContainElements("backup.run", "backup.step.postgres", "backup.step.objects", "backup.step.nats"))
			Expect(metricNames(h.tp)).To(ContainElements("backup.operations.total", "backup.step.duration_ms", "backup.bytes.total"))
		})

		DescribeTable("fails when backup-push adds the wrong number of new base backups, leaving the point incomplete",
			func(pushAdds int) {
				h.walg.pushAdds = pushAdds
				_, err := h.svc.Run(ctx)
				Expect(err).To(MatchError(ContainSubstring("new base backups (want 1)")))
				pts, err := h.repo.Points(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(pts.Complete).To(BeEmpty())
				Expect(pts.Incomplete).To(HaveLen(1))
				Expect(h.lock.released).To(Equal(1))
			},
			Entry("no new backups", 0),
			Entry("two new backups", 2),
		)

		It("fails at the objects step without leaving a manifest", func() {
			deps := h.deps
			deps.Tenants = erroringTenants{err: errors.New("tenant directory unavailable")}
			svc, err := backup.New(deps)
			Expect(err).NotTo(HaveOccurred())
			_, err = svc.Run(ctx)
			Expect(err).To(MatchError(ContainSubstring("objects")))
			Expect(err.Error()).To(ContainSubstring("tenant directory unavailable"))
			pts, err := h.repo.Points(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(pts.Complete).To(BeEmpty())
			Expect(pts.Incomplete).To(HaveLen(1))
			Expect(h.lock.released).To(Equal(1))
		})

		It("fails at the nats step without leaving a manifest", func() {
			delete(h.streams.streams, natsbus.AuditStreamNames()[0])
			_, err := h.svc.Run(ctx)
			Expect(err).To(MatchError(ContainSubstring("nats")))
			pts, err := h.repo.Points(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(pts.Complete).To(BeEmpty())
			Expect(pts.Incomplete).To(HaveLen(1))
			Expect(h.lock.released).To(Equal(1))
		})

		It("returns the manifest and an error naming the lock and point when release fails", func() {
			h.lock.releaseErr = errors.New("advisory unlock: connection reset")
			m, err := h.svc.Run(ctx)
			Expect(m).NotTo(BeNil())
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("lock"))
			Expect(err.Error()).To(ContainSubstring(m.ID))
			Expect(err.Error()).To(ContainSubstring("connection reset"))
		})
	})

	Describe("List", func() {
		It("separates complete and incomplete points", func() {
			first, err := h.svc.Run(ctx)
			Expect(err).NotTo(HaveOccurred())
			h.walg.pushErr = errors.New("boom")
			_, err = h.svc.Run(ctx)
			Expect(err).To(HaveOccurred())

			res, err := h.svc.List(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Complete).To(HaveLen(1))
			Expect(res.Complete[0].ID).To(Equal(first.ID))
			Expect(res.Incomplete).To(HaveLen(1))
		})
	})

	Describe("Verify", func() {
		It("passes for a fresh, intact point", func() {
			_, err := h.svc.Run(ctx)
			Expect(err).NotTo(HaveOccurred())
			rep, err := h.svc.Verify(ctx, backup.VerifyOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(rep.OK()).To(BeTrue())
			Expect(rep.Points).To(HaveLen(1))
			Expect(rep.Points[0].Issues).To(BeEmpty())
			Expect(rep.Points[0].EstimatedRestore).To(BeNumerically(">", 0))
			Expect(rep.WAL).To(ConsistOf(backup.WALCheck{Name: "integrity", Status: "OK"}, backup.WALCheck{Name: "timeline", Status: "OK"}))
			Expect(rep.Staleness).To(HaveLen(3))
			for _, s := range rep.Staleness {
				Expect(s.Stale).To(BeFalse(), s.Store)
			}
		})

		It("reports incomplete points", func() {
			_, err := h.svc.Run(ctx)
			Expect(err).NotTo(HaveOccurred())
			h.walg.pushErr = errors.New("boom")
			_, err = h.svc.Run(ctx)
			Expect(err).To(HaveOccurred())
			pts, err := h.repo.Points(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(pts.Incomplete).To(HaveLen(1))

			rep, err := h.svc.Verify(ctx, backup.VerifyOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(rep.Incomplete).To(Equal([]string{pts.Incomplete[0]}))
		})

		It("falls back to an older valid point for staleness when the newest manifest is corrupt", func() {
			first, err := h.svc.Run(ctx)
			Expect(err).NotTo(HaveOccurred())
			second, err := h.svc.Run(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(h.repoRoot, "backup", "points", second.ID, "manifest.json"), []byte("{}"), 0o600)).To(Succeed())

			rep, err := h.svc.Verify(ctx, backup.VerifyOptions{})
			Expect(err).NotTo(HaveOccurred())
			for _, s := range rep.Staleness {
				Expect(s.NewestPoint).To(Equal(first.ID), s.Store)
			}
		})

		It("fails on a tampered blob and counts the failure", func() {
			_, err := h.svc.Run(ctx)
			Expect(err).NotTo(HaveOccurred())
			h.tamperFirstBlob()
			rep, err := h.svc.Verify(ctx, backup.VerifyOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(rep.OK()).To(BeFalse())
			Expect(rep.Points[0].Issues).To(ContainElement(ContainSubstring("sha256 mismatch")))
			Expect(metricNames(h.tp)).To(ContainElement("backup.verify.failures.total"))
		})

		It("fails when the point's WAL-G base backup is gone", func() {
			m, err := h.svc.Run(ctx)
			Expect(err).NotTo(HaveOccurred())
			h.walg.backups = nil
			rep, err := h.svc.Verify(ctx, backup.VerifyOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(rep.OK()).To(BeFalse())
			Expect(rep.Points[0].Issues).To(ContainElement(ContainSubstring(m.Postgres.BackupName + " is missing")))
		})

		DescribeTable("treats a WAL FAILURE as fatal and a WARNING as informational",
			func(status string, ok bool) {
				_, err := h.svc.Run(ctx)
				Expect(err).NotTo(HaveOccurred())
				h.walg.walStatus = status
				rep, err := h.svc.Verify(ctx, backup.VerifyOptions{})
				Expect(err).NotTo(HaveOccurred())
				Expect(rep.OK()).To(Equal(ok))
			},
			Entry("WARNING", "WARNING", true),
			Entry("FAILURE", "FAILURE", false),
		)

		It("reports every store stale once the newest point is older than max_age", func() {
			_, err := h.svc.Run(ctx)
			Expect(err).NotTo(HaveOccurred())
			h.clock.t = h.clock.t.Add(2 * time.Hour)
			rep, err := h.svc.Verify(ctx, backup.VerifyOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(rep.OK()).To(BeFalse())
			for _, s := range rep.Staleness {
				Expect(s.Stale).To(BeTrue(), s.Store)
				Expect(s.Age).To(BeNumerically(">", time.Hour))
			}
		})

		It("treats an empty repository as stale everywhere", func() {
			rep, err := h.svc.Verify(ctx, backup.VerifyOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(rep.OK()).To(BeFalse())
			Expect(rep.Staleness).To(HaveEach(HaveField("Stale", BeTrue())))
		})

		It("reports a corrupt manifest as an issue, not a crash", func() {
			m, err := h.svc.Run(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(os.WriteFile(filepath.Join(h.repoRoot, "backup", "points", m.ID, "manifest.json"), []byte("{}"), 0o600)).To(Succeed())
			rep, err := h.svc.Verify(ctx, backup.VerifyOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(rep.OK()).To(BeFalse())
			Expect(rep.Points[0].Issues).To(ContainElement(ContainSubstring("invalid backup manifest")))
		})

		DescribeTable("rejects a --point that is not a complete point",
			func(id, want string) {
				h.walg.pushErr = errors.New("boom")
				_, _ = h.svc.Run(ctx) // leaves one incomplete point
				pts, err := h.repo.Points(ctx)
				Expect(err).NotTo(HaveOccurred())
				if id == "<incomplete>" {
					id = pts.Incomplete[0]
				}
				_, err = h.svc.Verify(ctx, backup.VerifyOptions{PointID: id})
				Expect(err).To(MatchError(ContainSubstring(want)))
			},
			Entry("incomplete", "<incomplete>", "is incomplete"),
			Entry("unknown", "20991231T000000Z-00000000", "does not exist"),
			Entry("malformed", "../etc", "invalid backup point ID"),
		)
	})

	Describe("Restore", func() {
		It("restores objects and streams into empty targets", func() {
			m, err := h.svc.Run(ctx)
			Expect(err).NotTo(HaveOccurred())
			svc, dst, dstStreams := h.restoreTarget()
			Expect(svc.Restore(ctx, m.ID)).To(Succeed())
			Expect(dst.contents("acme-corp", "globex")).To(Equal(h.src.contents("acme-corp", "globex")))
			Expect(dstStreams.streams).To(Equal(h.streams.streams))
		})

		It("refuses a non-empty tenant store and writes nothing anywhere", func() {
			m, err := h.svc.Run(ctx)
			Expect(err).NotTo(HaveOccurred())
			svc, dst, dstStreams := h.restoreTarget()
			dst.put("globex", "keep.json", "keep me")
			err = svc.Restore(ctx, m.ID)
			Expect(err).To(MatchError(ContainSubstring(`tenant "globex"`)))
			Expect(dst.contents("acme-corp", "globex")).To(Equal(map[string]map[string]string{
				"acme-corp": {}, "globex": {"keep.json": "keep me"},
			}))
			Expect(dstStreams.restored).To(BeEmpty())
		})

		It("refuses an existing stream and writes no objects", func() {
			m, err := h.svc.Run(ctx)
			Expect(err).NotTo(HaveOccurred())
			svc, dst, dstStreams := h.restoreTarget()
			dstStreams.streams["AUDIT_LLM"] = fakeStream{data: "live"}
			err = svc.Restore(ctx, m.ID)
			Expect(err).To(MatchError(ContainSubstring(`stream "AUDIT_LLM"`)))
			Expect(dst.contents("acme-corp", "globex")).To(Equal(map[string]map[string]string{"acme-corp": {}, "globex": {}}))
			Expect(dstStreams.streams["AUDIT_LLM"].data).To(Equal("live"))
		})

		It("refuses a point that fails verification and writes nothing", func() {
			m, err := h.svc.Run(ctx)
			Expect(err).NotTo(HaveOccurred())
			h.tamperFirstBlob()
			svc, dst, dstStreams := h.restoreTarget()
			err = svc.Restore(ctx, m.ID)
			Expect(err).To(MatchError(ContainSubstring("failed verification")))
			Expect(err.Error()).To(ContainSubstring("Nothing was written"))
			Expect(dst.contents("acme-corp", "globex")).To(Equal(map[string]map[string]string{"acme-corp": {}, "globex": {}}))
			Expect(dstStreams.restored).To(BeEmpty())
		})

		It("refuses an incomplete point", func() {
			h.walg.pushErr = errors.New("boom")
			_, _ = h.svc.Run(ctx)
			pts, err := h.repo.Points(ctx)
			Expect(err).NotTo(HaveOccurred())
			svc, _, _ := h.restoreTarget()
			Expect(svc.Restore(ctx, pts.Incomplete[0])).To(MatchError(ContainSubstring("is incomplete")))
		})

		It("reports a clear retry message when an object write fails mid-restore", func() {
			m, err := h.svc.Run(ctx)
			Expect(err).NotTo(HaveOccurred())
			dst := newObjectStores()
			deps := h.deps
			deps.Objects = failingPutOpener(dst.root)
			deps.Streams = newFakeStreamStore()
			svc, err := backup.New(deps)
			Expect(err).NotTo(HaveOccurred())
			err = svc.Restore(ctx, m.ID)
			Expect(err).To(MatchError(ContainSubstring("empty the tenant object stores")))
		})
	})
})
