package backup

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/telemetry"
)

// Locker keeps a backup run and a non-dry-run retention scan apart.
// db.AdvisoryLocker implements it with db.BackupRetentionLockName.
type Locker interface {
	TryAcquire(ctx context.Context) (release func(context.Context) error, err error)
}

// MaxAge is the staleness threshold per store (config backup.max_age).
type MaxAge struct {
	Postgres, Objects, NATS time.Duration
}

// Deps wires a Service. Each method uses only some of them:
//   - Run: Repo, WALG, Tenants, Objects, Streams, StreamNames, Lock
//   - List: Repo
//   - Verify: Repo, WALG, MaxAge
//   - Restore: Repo, WALG, Objects, Streams
type Deps struct {
	Repo        *Repository
	WALG        *WALG
	Tenants     TenantLister
	Objects     ObjectStoreOpener
	Streams     StreamStore
	StreamNames []string
	Lock        Locker
	MaxAge      MaxAge
	TempDir     string           // spool and snapshot files; "" = os.TempDir()
	Now         func() time.Time // nil = time.Now
	Rand        io.Reader        // point ID suffixes; nil = crypto/rand.Reader
	Tracer      trace.Tracer     // nil = telemetry.Instrumentation("pkg/backup")
	Meter       metric.Meter     // nil = telemetry.Instrumentation("pkg/backup")
}

// Service runs the backup commands.
type Service struct {
	d   Deps
	tel *instruments
}

// New returns a Service. Repo is required.
func New(d Deps) (*Service, error) {
	if d.Repo == nil {
		return nil, errors.New("backup: Deps.Repo is required")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Rand == nil {
		d.Rand = rand.Reader
	}
	if d.Tracer == nil || d.Meter == nil {
		tracer, meter := telemetry.Instrumentation("pkg/backup")
		if d.Tracer == nil {
			d.Tracer = tracer
		}
		if d.Meter == nil {
			d.Meter = meter
		}
	}
	tel, err := newInstruments(d.Tracer, d.Meter)
	if err != nil {
		return nil, err
	}
	return &Service{d: d, tel: tel}, nil
}

// Run captures Postgres, then objects, then JetStream into a new point and
// commits its manifest. Postgres goes first and is authoritative; the other
// stores may be slightly ahead of it, never behind (docs/dev/backup.md).
// Any failure leaves the point incomplete (no manifest). When the manifest
// is written but cleanup fails, Run returns the manifest and an error.
func (s *Service) Run(ctx context.Context) (m *Manifest, err error) {
	ctx, done := s.tel.op(ctx, "run")
	defer func() { done(err) }()

	release, err := s.d.Lock.TryAcquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot start: %w. A non-dry-run retention scan or another backup run holds the lock; retry when it finishes", err)
	}
	var id string
	defer func() {
		if rerr := release(context.WithoutCancel(ctx)); rerr != nil {
			err = errors.Join(err, lockReleaseError(id, rerr))
		}
	}()

	started := s.d.Now().UTC()
	id, err = NewPointID(started, s.d.Rand)
	if err != nil {
		return nil, err
	}
	if err := s.d.Repo.Begin(ctx, id, started); err != nil {
		return nil, err
	}
	pg, err := s.capturePostgres(ctx)
	if err != nil {
		return nil, fmt.Errorf("point %s: postgres: %w", id, err)
	}
	objs, err := s.captureObjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("point %s: objects: %w", id, err)
	}
	streams, err := s.captureStreams(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("point %s: nats: %w", id, err)
	}
	m = &Manifest{
		SchemaVersion: SchemaVersion, ID: id, StartedAt: started, FinishedAt: s.d.Now().UTC(),
		Postgres: pg, Objects: objs, NATS: streams,
	}
	if err := s.d.Repo.WriteManifest(ctx, m); err != nil {
		return nil, fmt.Errorf("point %s: %w", id, err)
	}
	if err := s.d.Repo.ClearInProgress(ctx, id); err != nil {
		return m, fmt.Errorf("point %s is complete, but %w (harmless: the manifest is authoritative; delete the marker by hand)", id, err)
	}
	return m, nil
}

// lockReleaseError reports a failed lock release. id is empty when the
// point was never assigned (acquisition failed before NewPointID ran, so
// there is nothing to point an operator at); otherwise it names the point
// so `backup verify --point <id>` can confirm whether the run it guarded
// actually completed.
func lockReleaseError(id string, rerr error) error {
	if id == "" {
		return fmt.Errorf("release backup/retention lock: %w", rerr)
	}
	return fmt.Errorf("release backup/retention lock (the lock may have been lost during the run; run `crosscodexd admin backup verify --point %s` to check point %s): %w", id, id, rerr)
}

func newStats(start, end time.Time, bytes int64) CaptureStats {
	cs := CaptureStats{Window: Window{Start: start, End: end}, Bytes: bytes}
	if secs := end.Sub(start).Seconds(); secs > 0 {
		cs.ThroughputBytesPerSec = float64(bytes) / secs
	}
	return cs
}

func (s *Service) capturePostgres(ctx context.Context) (pc PostgresCapture, err error) {
	ctx, done := s.tel.step(ctx, "postgres")
	defer func() { done(pc.Bytes, err) }()
	before, err := s.d.WALG.Backups(ctx)
	if err != nil {
		return PostgresCapture{}, err
	}
	start := s.d.Now().UTC()
	if err := s.d.WALG.BackupPush(ctx); err != nil {
		return PostgresCapture{}, err
	}
	end := s.d.Now().UTC()
	after, err := s.d.WALG.Backups(ctx)
	if err != nil {
		return PostgresCapture{}, err
	}
	b, err := findNewBackup(before, after)
	if err != nil {
		return PostgresCapture{}, err
	}
	tl, err := b.Timeline()
	if err != nil {
		return PostgresCapture{}, err
	}
	return PostgresCapture{
		BackupName: b.Name, StartLSN: b.StartLSN, FinishLSN: b.FinishLSN, Timeline: tl,
		CaptureStats: newStats(start, end, b.CompressedSize),
	}, nil
}

// findNewBackup returns the one backup in after that is not in before.
// Comparing listings rather than timestamps keeps clock skew out of it.
func findNewBackup(before, after []WALGBackup) (WALGBackup, error) {
	known := make(map[string]bool, len(before))
	for _, b := range before {
		known[b.Name] = true
	}
	var fresh []WALGBackup
	for _, b := range after {
		if !known[b.Name] {
			fresh = append(fresh, b)
		}
	}
	if len(fresh) != 1 {
		return WALGBackup{}, fmt.Errorf("wal-g backup-push finished but backup-list shows %d new base backups (want 1); another backup-push may have run at the same time. Retry", len(fresh))
	}
	return fresh[0], nil
}

func (s *Service) captureObjects(ctx context.Context) (oc ObjectsCapture, err error) {
	ctx, done := s.tel.step(ctx, "objects")
	defer func() { done(oc.Bytes, err) }()
	start := s.d.Now().UTC()
	tenants, err := s.d.Tenants.TenantIDs(ctx)
	if err != nil {
		return ObjectsCapture{}, err
	}
	byTenant, total, err := snapshotObjects(ctx, s.d.Repo, tenants, s.d.Objects, s.d.TempDir)
	if err != nil {
		return ObjectsCapture{}, err
	}
	return ObjectsCapture{Tenants: byTenant, CaptureStats: newStats(start, s.d.Now().UTC(), total)}, nil
}

func (s *Service) captureStreams(ctx context.Context, pointID string) (nc NATSCapture, err error) {
	ctx, done := s.tel.step(ctx, "nats")
	defer func() { done(nc.Bytes, err) }()
	start := s.d.Now().UTC()
	caps, total, err := snapshotStreams(ctx, s.d.Repo, pointID, s.d.Streams, s.d.StreamNames, s.d.TempDir)
	if err != nil {
		return NATSCapture{}, err
	}
	return NATSCapture{Streams: caps, CaptureStats: newStats(start, s.d.Now().UTC(), total)}, nil
}

// ListResult is the output of List.
type ListResult struct {
	Complete   []*Manifest // ascending by point ID (time)
	Incomplete []string
}

// List returns every point. A complete point whose manifest fails to decode
// is an error; `verify` reports the details.
func (s *Service) List(ctx context.Context) (res ListResult, err error) {
	ctx, done := s.tel.op(ctx, "list")
	defer func() { done(err) }()
	pts, err := s.d.Repo.Points(ctx)
	if err != nil {
		return ListResult{}, err
	}
	for _, id := range pts.Complete {
		m, err := s.d.Repo.LoadManifest(ctx, id)
		if err != nil {
			return ListResult{}, fmt.Errorf("%w. Run `crosscodexd admin backup verify --point %s` for details", err, id)
		}
		res.Complete = append(res.Complete, m)
	}
	res.Incomplete = pts.Incomplete
	return res, nil
}

// VerifyOptions selects the points Verify checks; PointID "" means all
// complete points. Staleness is always computed over the whole repository
// (the newest valid point), even when PointID is set, so `verify --point`
// can fail on staleness alone.
type VerifyOptions struct {
	PointID string
}

// PointReport is one point's verification result.
type PointReport struct {
	ID               string
	Issues           []string      // integrity problems; empty means intact
	EstimatedRestore time.Duration // captured bytes / capture throughput; 0 when unknown
}

// StoreAge is one store's staleness, measured from its capture window in
// the newest valid point.
type StoreAge struct {
	Store       string
	NewestPoint string // "" when no valid point exists
	Age         time.Duration
	MaxAge      time.Duration
	Stale       bool
}

// VerifyReport is the output of Verify.
type VerifyReport struct {
	Points     []PointReport
	Incomplete []string
	WAL        []WALCheck
	Staleness  []StoreAge
}

// OK is true when every checked point is intact, no WAL check reports
// FAILURE, and no store is stale.
func (r VerifyReport) OK() bool {
	for _, p := range r.Points {
		if len(p.Issues) > 0 {
			return false
		}
	}
	for _, c := range r.WAL {
		if c.Status == WALStatusFailure {
			return false
		}
	}
	for _, s := range r.Staleness {
		if s.Stale {
			return false
		}
	}
	return true
}

// Verify checks blob and snapshot checksums, WAL-G base backup presence,
// WAL continuity (wal-g wal-verify) and staleness.
func (s *Service) Verify(ctx context.Context, opts VerifyOptions) (rep VerifyReport, err error) {
	ctx, done := s.tel.op(ctx, "verify")
	defer func() { done(err) }()
	pts, err := s.d.Repo.Points(ctx)
	if err != nil {
		return VerifyReport{}, err
	}
	ids := pts.Complete
	if opts.PointID != "" {
		if err := requireComplete(pts, opts.PointID); err != nil {
			return VerifyReport{}, err
		}
		ids = []string{opts.PointID}
	}
	names, err := s.walgBackupNames(ctx)
	if err != nil {
		return VerifyReport{}, err
	}
	loaded := map[string]*Manifest{}
	for _, id := range ids {
		pr, m, err := s.verifyPoint(ctx, id, names)
		if err != nil {
			return VerifyReport{}, fmt.Errorf("verify point %s: %w", id, err)
		}
		rep.Points = append(rep.Points, pr)
		if m != nil {
			loaded[id] = m
		}
	}
	if rep.WAL, err = s.d.WALG.VerifyWAL(ctx); err != nil {
		return VerifyReport{}, err
	}
	newest, err := s.newestManifest(ctx, pts.Complete, loaded)
	if err != nil {
		return VerifyReport{}, err
	}
	rep.Staleness = staleness(newest, s.d.MaxAge, s.d.Now().UTC())
	rep.Incomplete = pts.Incomplete
	s.tel.recordVerify(ctx, rep)
	return rep, nil
}

func requireComplete(pts PointList, id string) error {
	if err := ValidatePointID(id); err != nil {
		return err
	}
	if slices.Contains(pts.Complete, id) {
		return nil
	}
	if slices.Contains(pts.Incomplete, id) {
		return fmt.Errorf("backup point %s is incomplete (its run did not finish); choose a complete point from `crosscodexd admin backup list`", id)
	}
	return fmt.Errorf("backup point %s does not exist; choose a complete point from `crosscodexd admin backup list`", id)
}

func (s *Service) walgBackupNames(ctx context.Context) (map[string]bool, error) {
	list, err := s.d.WALG.Backups(ctx)
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(list))
	for _, b := range list {
		names[b.Name] = true
	}
	return names, nil
}

// verifyPoint checks one point. A manifest that fails to decode is an
// issue (m is nil); I/O failures are errors.
func (s *Service) verifyPoint(ctx context.Context, id string, walgBackups map[string]bool) (PointReport, *Manifest, error) {
	pr := PointReport{ID: id}
	m, err := s.d.Repo.LoadManifest(ctx, id)
	if errors.Is(err, ErrInvalidManifest) {
		pr.Issues = append(pr.Issues, err.Error())
		return pr, nil, nil
	}
	if err != nil {
		return pr, nil, err
	}
	if !walgBackups[m.Postgres.BackupName] {
		pr.Issues = append(pr.Issues, fmt.Sprintf("WAL-G base backup %s is missing from the destination", m.Postgres.BackupName))
	}
	issues, err := verifyObjects(ctx, s.d.Repo, m.Objects.Tenants)
	if err != nil {
		return pr, nil, err
	}
	pr.Issues = append(pr.Issues, issues...)
	issues, err = verifyStreams(ctx, s.d.Repo, id, m.NATS.Streams)
	if err != nil {
		return pr, nil, err
	}
	pr.Issues = append(pr.Issues, issues...)
	pr.EstimatedRestore = estimateRestore(m)
	return pr, m, nil
}

// newestManifest returns the newest complete point whose manifest decodes;
// a corrupt newest point must not make the stores look fresh.
func (s *Service) newestManifest(ctx context.Context, complete []string, loaded map[string]*Manifest) (*Manifest, error) {
	for i := len(complete) - 1; i >= 0; i-- {
		if m, ok := loaded[complete[i]]; ok {
			return m, nil
		}
		m, err := s.d.Repo.LoadManifest(ctx, complete[i])
		if errors.Is(err, ErrInvalidManifest) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return m, nil
	}
	return nil, nil
}

func staleness(newest *Manifest, maxAge MaxAge, now time.Time) []StoreAge {
	stores := []struct {
		name string
		max  time.Duration
		end  func(*Manifest) time.Time
	}{
		{"postgres", maxAge.Postgres, func(m *Manifest) time.Time { return m.Postgres.Window.End }},
		{"objects", maxAge.Objects, func(m *Manifest) time.Time { return m.Objects.Window.End }},
		{"nats", maxAge.NATS, func(m *Manifest) time.Time { return m.NATS.Window.End }},
	}
	out := make([]StoreAge, 0, len(stores))
	for _, st := range stores {
		sa := StoreAge{Store: st.name, MaxAge: st.max, Stale: true}
		if newest != nil {
			sa.NewestPoint = newest.ID
			sa.Age = now.Sub(st.end(newest))
			sa.Stale = sa.Age > st.max
		}
		out = append(out, sa)
	}
	return out
}

// estimateRestore assumes restore runs at the capture throughput.
func estimateRestore(m *Manifest) time.Duration {
	var secs float64
	for _, cs := range []CaptureStats{m.Postgres.CaptureStats, m.Objects.CaptureStats, m.NATS.CaptureStats} {
		if cs.ThroughputBytesPerSec > 0 {
			secs += float64(cs.Bytes) / cs.ThroughputBytesPerSec
		}
	}
	return time.Duration(secs * float64(time.Second))
}

// Restore restores a point's objects and streams. Postgres must already be
// restored with crosscodex-db-restore (see deploy/README.md). The point is
// verified first and every target is checked empty before anything is
// written, so a refusal changes nothing.
func (s *Service) Restore(ctx context.Context, pointID string) (err error) {
	ctx, done := s.tel.op(ctx, "restore")
	defer func() { done(err) }()
	pts, err := s.d.Repo.Points(ctx)
	if err != nil {
		return err
	}
	if err := requireComplete(pts, pointID); err != nil {
		return err
	}
	names, err := s.walgBackupNames(ctx)
	if err != nil {
		return err
	}
	pr, m, err := s.verifyPoint(ctx, pointID, names)
	if err != nil {
		return err
	}
	if len(pr.Issues) > 0 {
		return fmt.Errorf("refusing to restore point %s: it failed verification:\n  %s\nNothing was written. Choose another point from `crosscodexd admin backup list`",
			pointID, strings.Join(pr.Issues, "\n  "))
	}
	if err := preflightObjects(ctx, m.Objects.Tenants, s.d.Objects); err != nil {
		return err
	}
	if err := preflightStreams(ctx, m.NATS.Streams, s.d.Streams); err != nil {
		return err
	}

	octx, endObjects := s.tel.span(ctx, "backup.restore.objects")
	err = restoreObjects(octx, s.d.Repo, m.Objects.Tenants, s.d.Objects, s.d.TempDir)
	endObjects(err)
	if err != nil {
		return fmt.Errorf("restore point %s: objects: %w. Some objects may already be written; empty the tenant object stores before retrying", pointID, err)
	}
	nctx, endStreams := s.tel.span(ctx, "backup.restore.nats")
	err = restoreStreams(nctx, s.d.Repo, pointID, m.NATS.Streams, s.d.Streams, s.d.TempDir)
	endStreams(err)
	if err != nil {
		return fmt.Errorf("restore point %s: nats: %w. Objects are already restored; to retry, empty the tenant object stores and delete any restored audit streams first", pointID, err)
	}
	return nil
}
