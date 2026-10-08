package retention_test

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/retention"
)

// --- Engine test fakes (all names prefixed engineFake to avoid redeclaration) ---

type engineFakeCollector struct {
	candidates []retention.Candidate
}

func (f *engineFakeCollector) Collect(_ context.Context, _ retention.Policy, _ time.Time) ([]retention.Candidate, error) {
	return f.candidates, nil
}

// engineFakeHoldStore implements the full HoldStore interface. Only Active is
// exercised by the Engine; the unused methods return zero values, which is
// acceptable for a test double.
type engineFakeHoldStore struct {
	active []retention.Hold
}

func (f *engineFakeHoldStore) Active(_ context.Context, _ time.Time) ([]retention.Hold, error) {
	return f.active, nil
}
func (f *engineFakeHoldStore) Create(_ context.Context, h retention.Hold) (retention.Hold, error) {
	return h, nil
}
func (f *engineFakeHoldStore) Release(_ context.Context, _, _ string) error { return nil }
func (f *engineFakeHoldStore) List(_ context.Context) ([]retention.Hold, error) {
	return nil, nil
}

// engineFakeArchiver records successful Archive calls. Entries in errFor (keyed
// by candidate ID) cause Archive to return the mapped error without recording
// the call. When orderLog is non-nil, each successful call appends
// "archive:<ID>" to the shared slice for ordering assertions.
type engineFakeArchiver struct {
	calls    []string
	errFor   map[string]error
	orderLog *[]string
}

func (f *engineFakeArchiver) Archive(_ context.Context, c retention.Candidate) error {
	if f.errFor != nil {
		if err, ok := f.errFor[c.ID]; ok {
			return err
		}
	}
	f.calls = append(f.calls, c.ID)
	if f.orderLog != nil {
		*f.orderLog = append(*f.orderLog, "archive:"+c.ID)
	}
	return nil
}

// engineFakePurger records Purge calls. When orderLog is non-nil, each call
// appends "purge:<ID>" to the shared slice for ordering assertions.
type engineFakePurger struct {
	calls    []string
	orderLog *[]string
}

func (f *engineFakePurger) Purge(_ context.Context, c retention.Candidate) error {
	f.calls = append(f.calls, c.ID)
	if f.orderLog != nil {
		*f.orderLog = append(*f.orderLog, "purge:"+c.ID)
	}
	return nil
}

// engineFakeAuditPublisher captures Publish calls. When failPublish is true
// every Publish call returns an error (simulates a failing audit bus).
type engineFakeAuditPublisher struct {
	records     []retention.AuditRecord
	failPublish bool
}

func (f *engineFakeAuditPublisher) Publish(_ context.Context, r retention.AuditRecord) error {
	if f.failPublish {
		return fmt.Errorf("audit bus unavailable")
	}
	f.records = append(f.records, r)
	return nil
}

// --- Specs ---

var _ = Describe("Engine.Scan", func() {
	var (
		ctx       context.Context
		now       time.Time
		c1, c2    retention.Candidate
		collector *engineFakeCollector
		holds     *engineFakeHoldStore
		archiver  *engineFakeArchiver
		purger    *engineFakePurger
		publisher *engineFakeAuditPublisher
		eng       *retention.Engine
	)

	BeforeEach(func() {
		ctx = context.Background()
		now = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

		c1 = retention.Candidate{
			Store:    retention.StorePostgres,
			Class:    retention.ClassJobResults,
			ID:       "job-c1",
			TenantID: "tenant1",
		}
		c2 = retention.Candidate{
			Store:    retention.StorePostgres,
			Class:    retention.ClassJobResults,
			ID:       "job-c2",
			TenantID: "tenant1",
		}

		// Hold with a JobID scope that covers c2 but not c1.
		holdC2 := retention.Hold{
			ID:   "hold-1",
			Name: "legal-hold-c2",
			Scope: retention.HoldScope{
				JobID: "job-c2",
			},
		}

		collector = &engineFakeCollector{candidates: []retention.Candidate{c1, c2}}
		holds = &engineFakeHoldStore{active: []retention.Hold{holdC2}}
		archiver = &engineFakeArchiver{}
		purger = &engineFakePurger{}
		publisher = &engineFakeAuditPublisher{}
		// Policy zero value is intentional: the fake collector ignores it and
		// the engine does not call Policy.Expired directly.
		eng = retention.NewEngine([]retention.Collector{collector}, holds, archiver, purger, publisher, retention.Policy{}, &fakeScanLock{})
	})

	It("archives then purges non-held expired candidates", func() {
		rep, err := eng.Scan(ctx, retention.ScanOptions{Now: now})

		Expect(err).NotTo(HaveOccurred())
		Expect(rep.Scanned).To(Equal(2))
		Expect(rep.Held).To(Equal(1))
		Expect(rep.Archived).To(Equal(1))
		Expect(rep.Purged).To(Equal(1))
		// c2 is under legal hold — archiver and purger must never see it.
		Expect(archiver.calls).To(ConsistOf("job-c1"))
		Expect(purger.calls).To(ConsistOf("job-c1"))
		// Audit: one ActionArchive + one ActionPurge record for c1.
		Expect(publisher.records).To(HaveLen(2))
		var actions []string
		for _, r := range publisher.records {
			actions = append(actions, r.Action)
		}
		Expect(actions).To(ConsistOf(retention.ActionArchive, retention.ActionPurge))
		// Actor and TenantID are correct on every record.
		for _, r := range publisher.records {
			Expect(r.Actor).To(Equal("retention-engine"))
			Expect(r.TenantID).To(Equal("tenant1"))
		}
	})

	It("reports correct PerClass tallies", func() {
		rep, err := eng.Scan(ctx, retention.ScanOptions{Now: now})

		Expect(err).NotTo(HaveOccurred())
		cc := rep.PerClass[retention.ClassJobResults]
		Expect(cc.Scanned).To(Equal(2))
		Expect(cc.Held).To(Equal(1))
		Expect(cc.Archived).To(Equal(1))
		Expect(cc.Purged).To(Equal(1))
	})

	It("dry-run reports scanned count but never calls archiver or purger", func() {
		rep, err := eng.Scan(ctx, retention.ScanOptions{Now: now, DryRun: true})

		Expect(err).NotTo(HaveOccurred())
		Expect(rep.Scanned).To(BeNumerically(">", 0))
		Expect(archiver.calls).To(BeEmpty())
		Expect(purger.calls).To(BeEmpty())
		Expect(publisher.records).To(BeEmpty())
	})

	It("uses time.Now() when opts.Now is zero", func() {
		rep, err := eng.Scan(ctx, retention.ScanOptions{})

		Expect(err).NotTo(HaveOccurred())
		Expect(rep.Scanned).To(Equal(2))
	})

	It("archive failure skips purge and records the error", func() {
		archiver.errFor = map[string]error{
			"job-c1": retention.ErrArchiveVerify,
		}

		rep, err := eng.Scan(ctx, retention.ScanOptions{Now: now})

		Expect(err).NotTo(HaveOccurred())
		Expect(rep.Archived).To(Equal(0))
		Expect(rep.Purged).To(Equal(0))
		Expect(purger.calls).To(BeEmpty())
		// The failed archive attempt must not appear in successful calls.
		Expect(archiver.calls).To(BeEmpty())
		Expect(rep.Errors).To(HaveLen(1))
		Expect(rep.Errors[0]).To(ContainSubstring("archive"))
	})

	// Ruling S else-branch: object candidates must never pass a delimiter-bearing
	// token to the NATS subject builder. ClassAttestation has ObjectKey
	// "attestation/scan.json" (contains "."); auditJobID must return "retention".
	It("object candidate audit uses delimiter-free JobID routing token", func() {
		objCandidate := retention.Candidate{
			Store:     retention.StoreObject,
			Class:     retention.ClassAttestation,
			ID:        "att-1",
			ObjectKey: "attestation/scan.json",
			TenantID:  "t1",
			CreatedAt: now.Add(-2 * 365 * 24 * time.Hour),
		}
		objCollector := &engineFakeCollector{candidates: []retention.Candidate{objCandidate}}
		objHolds := &engineFakeHoldStore{}
		objArchiver := &engineFakeArchiver{}
		objPurger := &engineFakePurger{}
		objPublisher := &engineFakeAuditPublisher{}
		objEng := retention.NewEngine(
			[]retention.Collector{objCollector}, objHolds,
			objArchiver, objPurger, objPublisher, retention.Policy{}, &fakeScanLock{},
		)

		rep, err := objEng.Scan(ctx, retention.ScanOptions{Now: now})

		Expect(err).NotTo(HaveOccurred())
		Expect(rep.Archived).To(Equal(1))
		Expect(rep.Purged).To(Equal(1))
		Expect(objPublisher.records).To(HaveLen(2))
		for _, r := range objPublisher.records {
			// Must be the safe routing token, NOT the raw key (which contains ".").
			Expect(r.JobID).To(Equal("retention"))
			Expect(r.DataClass).To(Equal("attestation"))
			// The real identity must be in Detail.
			Expect(r.Detail).To(Equal("attestation/scan.json"))
		}
	})

	// Ruling T: audit-publish failure is non-fatal. Destructive work (archive +
	// purge) completes; the publish error is recorded in rep.Errors only.
	It("audit publish failure is non-fatal and does not undo archive or purge", func() {
		publisher.failPublish = true
		// Use only c1 (no holds in this sub-scenario) — rebuild engine with
		// a collector that yields only c1 so tallies are unambiguous.
		c1Only := &engineFakeCollector{candidates: []retention.Candidate{c1}}
		noHolds := &engineFakeHoldStore{}
		localEng := retention.NewEngine(
			[]retention.Collector{c1Only}, noHolds,
			archiver, purger, publisher, retention.Policy{}, &fakeScanLock{},
		)

		rep, err := localEng.Scan(ctx, retention.ScanOptions{Now: now})

		Expect(err).NotTo(HaveOccurred())
		// Destructive steps completed despite audit failure.
		Expect(rep.Archived).To(Equal(1))
		Expect(rep.Purged).To(Equal(1))
		// Each audit publish returned an error — two per candidate (archive + purge).
		Expect(rep.Errors).To(HaveLen(2))
		for _, e := range rep.Errors {
			Expect(e).To(ContainSubstring("audit publish"))
		}
	})

	// Ordering: archive must always precede purge for the same candidate.
	It("archives before purging every candidate", func() {
		var log []string
		archiver.orderLog = &log
		purger.orderLog = &log
		// Use only c1 so the log has exactly two entries.
		c1Only := &engineFakeCollector{candidates: []retention.Candidate{c1}}
		noHolds := &engineFakeHoldStore{}
		orderedEng := retention.NewEngine(
			[]retention.Collector{c1Only}, noHolds,
			archiver, purger, publisher, retention.Policy{}, &fakeScanLock{},
		)

		_, err := orderedEng.Scan(ctx, retention.ScanOptions{Now: now})

		Expect(err).NotTo(HaveOccurred())
		Expect(log).To(HaveLen(2))
		Expect(log[0]).To(Equal("archive:job-c1"))
		Expect(log[1]).To(Equal("purge:job-c1"))
	})

	Describe("scan lock", func() {
		newLockedEngine := func(lock retention.ScanLock) *retention.Engine {
			return retention.NewEngine([]retention.Collector{collector}, holds, archiver, purger, publisher, retention.Policy{}, lock)
		}

		It("holds the lock for a non-dry-run scan and releases it once", func() {
			lock := &fakeScanLock{}
			_, err := newLockedEngine(lock).Scan(ctx, retention.ScanOptions{Now: now})
			Expect(err).NotTo(HaveOccurred())
			Expect(lock.acquired).To(Equal(1))
			Expect(lock.released).To(Equal(1))
		})

		It("refuses to archive or purge while a backup holds the lock", func() {
			lock := &fakeScanLock{heldErr: fmt.Errorf("advisory lock %q: %w", db.BackupRetentionLockName, db.ErrLockHeld)}
			_, err := newLockedEngine(lock).Scan(ctx, retention.ScanOptions{Now: now})
			Expect(err).To(MatchError(db.ErrLockHeld))
			Expect(err.Error()).To(ContainSubstring("--dry-run"))
			Expect(archiver.calls).To(BeEmpty(), "nothing archived")
			Expect(purger.calls).To(BeEmpty(), "nothing purged")
			Expect(publisher.records).To(BeEmpty(), "nothing audited")
		})

		It("does not take the lock for a dry run", func() {
			lock := &fakeScanLock{heldErr: db.ErrLockHeld}
			rep, err := newLockedEngine(lock).Scan(ctx, retention.ScanOptions{Now: now, DryRun: true})
			Expect(err).NotTo(HaveOccurred())
			Expect(rep.Scanned).To(Equal(2))
		})

		It("fails closed without a lock", func() {
			_, err := newLockedEngine(nil).Scan(ctx, retention.ScanOptions{Now: now})
			Expect(err).To(MatchError(ContainSubstring("no scan lock")))
			Expect(archiver.calls).To(BeEmpty())
			Expect(purger.calls).To(BeEmpty())
		})
	})
})

// fakeScanLock records acquisitions; heldErr makes TryAcquire fail.
type fakeScanLock struct {
	heldErr  error
	acquired int
	released int
}

func (l *fakeScanLock) TryAcquire(context.Context) (func(context.Context) error, error) {
	if l.heldErr != nil {
		return nil, l.heldErr
	}
	l.acquired++
	return func(context.Context) error { l.released++; return nil }, nil
}
