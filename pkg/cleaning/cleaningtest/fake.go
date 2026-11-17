package cleaningtest

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ cleaning.FindingStore = (*Fake)(nil)

// Reviewer statuses Fake.Reviewers holds, as reviewers.status does.
const (
	ReviewerActive  = "active"
	ReviewerDeleted = "deleted"
)

// Fake fakes cleaning.FindingStore. See the package documentation for the
// semantics it mirrors. The zero value is ready to use. Set the exported
// fields before the first call; the methods are then safe for concurrent use.
type Fake struct {
	// Seed maps a tenant to findings present before the first call, stored
	// as given. Read once.
	Seed map[string][]cleaning.StoredFinding
	// Jobs maps a tenant to the jobs CompletedJobs returns.
	Jobs map[string][]cleaning.Job
	// Reviewers maps a tenant to its reviewers' IDs and statuses
	// (ReviewerActive or ReviewerDeleted).
	Reviewers map[string]map[string]string
	// Now stamps UpdatedAt and a zero Decision.DecidedAt. Nil means time.Now.
	Now func() time.Time

	// CompletedJobsFunc, if set, alone decides what CompletedJobs returns.
	CompletedJobsFunc func(ctx context.Context, tenantID string) ([]cleaning.Job, error)
	// FindingsFunc, if set, alone decides what Findings returns.
	FindingsFunc func(ctx context.Context, tenantID string, scope cleaning.Scope) ([]cleaning.StoredFinding, error)
	// ListFindingsFunc, if set, alone decides what ListFindings returns.
	ListFindingsFunc func(ctx context.Context, tenantID string, f cleaning.FindingFilter) ([]cleaning.StoredFinding, string, error)
	// GetFindingFunc, if set, alone decides what GetFinding returns.
	GetFindingFunc func(ctx context.Context, tenantID, findingID string) (cleaning.StoredFinding, []cleaning.Decision, error)
	// ApplyRunFunc, if set, alone decides what ApplyRun returns; nothing is stored.
	ApplyRunFunc func(ctx context.Context, tenantID string, w cleaning.RunWrite) error
	// DecideFunc, if set, alone decides what Decide returns; nothing is stored.
	DecideFunc func(ctx context.Context, tenantID string, d cleaning.Decision, expectState string, next cleaning.StoredFinding) error
	// UnprojectedFunc, if set, alone decides what Unprojected returns.
	UnprojectedFunc func(ctx context.Context, tenantID string) ([]cleaning.StoredFinding, error)
	// MarkProjectedFunc, if set, alone decides what MarkProjected returns;
	// nothing changes.
	MarkProjectedFunc func(ctx context.Context, tenantID, findingID string, edgeIDs []string) error
	// PendingCountFunc, if set, alone decides what PendingCount returns.
	PendingCountFunc func(ctx context.Context, tenantID string) (int64, error)

	mu        sync.Mutex
	loaded    bool
	findings  map[string]map[string]cleaning.StoredFinding // tenant -> ID -> finding; never aliased outside the Fake
	decisions map[string][]cleaning.Decision               // tenant -> decisions in insertion order
}

// checkTenant mirrors the real store's inTx: ctx must carry a tenant equal
// to tenantID.
func checkTenant(ctx context.Context, tenantID string) error {
	ctxTenant, err := tenant.FromContext(ctx)
	if err != nil {
		return fmt.Errorf("finding store: %w", err)
	}
	if ctxTenant != tenantID {
		return fmt.Errorf("finding store: context tenant %q does not match tenant %q", ctxTenant, tenantID)
	}
	return nil
}

// load copies Seed into the store on first use. Callers hold f.mu.
func (f *Fake) load() {
	if f.loaded {
		return
	}
	f.loaded = true
	f.findings = map[string]map[string]cleaning.StoredFinding{}
	f.decisions = map[string][]cleaning.Decision{}
	for tenantID, seeded := range f.Seed {
		f.findings[tenantID] = map[string]cleaning.StoredFinding{}
		for _, s := range seeded {
			f.findings[tenantID][s.ID] = cloneFinding(s)
		}
	}
}

// now returns the time the database would stamp: microsecond precision, UTC.
func (f *Fake) now() time.Time {
	t := time.Now()
	if f.Now != nil {
		t = f.Now()
	}
	return t.Round(time.Microsecond).UTC()
}

// sorted returns copies of the tenant's findings keep accepts, sorted by ID.
// Callers hold f.mu.
func (f *Fake) sorted(tenantID string, keep func(cleaning.StoredFinding) bool) []cleaning.StoredFinding {
	var out []cleaning.StoredFinding
	for _, s := range f.findings[tenantID] {
		if keep(s) {
			out = append(out, cloneFinding(s))
		}
	}
	slices.SortFunc(out, func(x, y cleaning.StoredFinding) int { return cmp.Compare(x.ID, y.ID) })
	return out
}

// covers mirrors Scope's unordered catalog-pair rule.
func covers(source, target string, f cleaning.StoredFinding) bool {
	if source == "" && target == "" {
		return true
	}
	return (f.SourceCatalog == source && f.TargetCatalog == target) || (f.SourceCatalog == target && f.TargetCatalog == source)
}

// checkPair mirrors the real store's refusal of a one-sided catalog pair.
func checkPair(source, target string) error {
	if (source == "") != (target == "") {
		return fmt.Errorf("catalog pair %q/%q has one side only: set both the source and the target catalog, or neither", source, target)
	}
	return nil
}

// CompletedJobs returns copies of Jobs[tenantID] sorted by ID.
func (f *Fake) CompletedJobs(ctx context.Context, tenantID string) ([]cleaning.Job, error) {
	if err := checkTenant(ctx, tenantID); err != nil {
		return nil, err
	}
	if f.CompletedJobsFunc != nil {
		return f.CompletedJobsFunc(ctx, tenantID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []cleaning.Job
	for _, j := range f.Jobs[tenantID] {
		c := j
		if j.Stages != nil {
			c.Stages = make(map[string]*cleaning.StageRecord, len(j.Stages))
			for name, r := range j.Stages {
				if r != nil {
					rc := *r
					rc.Models = maps.Clone(r.Models)
					r = &rc
				}
				c.Stages[name] = r
			}
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(x, y cleaning.Job) int { return cmp.Compare(x.ID, y.ID) })
	return out, nil
}

// Findings returns the tenant's findings scope covers, sorted by ID.
func (f *Fake) Findings(ctx context.Context, tenantID string, scope cleaning.Scope) ([]cleaning.StoredFinding, error) {
	if err := checkTenant(ctx, tenantID); err != nil {
		return nil, err
	}
	if f.FindingsFunc != nil {
		return f.FindingsFunc(ctx, tenantID, scope)
	}
	if err := checkPair(scope.SourceCatalog, scope.TargetCatalog); err != nil {
		return nil, fmt.Errorf("findings: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.load()
	return f.sorted(tenantID, func(s cleaning.StoredFinding) bool {
		return covers(scope.SourceCatalog, scope.TargetCatalog, s)
	}), nil
}

// ListFindings returns one page of matching findings, keyed on ID.
func (f *Fake) ListFindings(ctx context.Context, tenantID string, ff cleaning.FindingFilter) ([]cleaning.StoredFinding, string, error) {
	if err := checkTenant(ctx, tenantID); err != nil {
		return nil, "", err
	}
	if f.ListFindingsFunc != nil {
		return f.ListFindingsFunc(ctx, tenantID, ff)
	}
	if ff.PageSize < 1 {
		return nil, "", fmt.Errorf("list findings: page size %d is not positive; pass at least 1", ff.PageSize)
	}
	if err := checkPair(ff.SourceCatalog, ff.TargetCatalog); err != nil {
		return nil, "", fmt.Errorf("list findings: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.load()
	page := f.sorted(tenantID, func(s cleaning.StoredFinding) bool {
		return (ff.Signal == "" || s.Signal == ff.Signal) && (ff.Status == "" || s.Status == ff.Status) &&
			covers(ff.SourceCatalog, ff.TargetCatalog, s) && s.ID > ff.PageToken
	})
	if len(page) <= ff.PageSize {
		return page, "", nil
	}
	page = page[:ff.PageSize]
	return page, page[len(page)-1].ID, nil
}

// GetFinding returns a finding and its decisions, oldest first.
func (f *Fake) GetFinding(ctx context.Context, tenantID, findingID string) (cleaning.StoredFinding, []cleaning.Decision, error) {
	if err := checkTenant(ctx, tenantID); err != nil {
		return cleaning.StoredFinding{}, nil, err
	}
	if f.GetFindingFunc != nil {
		return f.GetFindingFunc(ctx, tenantID, findingID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.load()
	s, ok := f.findings[tenantID][findingID]
	if !ok {
		return cleaning.StoredFinding{}, nil, fmt.Errorf("finding %s: %w", findingID, cleaning.ErrFindingNotFound)
	}
	var ds []cleaning.Decision
	for _, d := range f.decisions[tenantID] {
		if d.FindingID == findingID {
			ds = append(ds, cloneDecision(d))
		}
	}
	slices.SortStableFunc(ds, func(x, y cleaning.Decision) int {
		return cmp.Or(x.DecidedAt.Compare(y.DecidedAt), cmp.Compare(x.ID, y.ID))
	})
	return cloneFinding(s), ds, nil
}

// staleRun is the refusal of an upsert whose expected state no longer holds;
// the real store words it the same way.
func staleRun(id string) error {
	return fmt.Errorf("finding %s changed since the preview; nothing was written. Preview the run again and apply its new plan: %w",
		id, cleaning.ErrStateChanged)
}

// insertDecision appends d to ds as edge_finding_decisions would accept it:
// a stored finding (foreign key) and an ID not already stored (primary key).
// The caller has validated d.
func (f *Fake) insertDecision(ds []cleaning.Decision, found map[string]cleaning.StoredFinding, d cleaning.Decision) ([]cleaning.Decision, error) {
	if _, ok := found[d.FindingID]; !ok {
		return nil, fmt.Errorf("decision %s refers to finding %s, which is not stored; upsert the finding in the same run; nothing was written",
			d.ID, d.FindingID)
	}
	if slices.ContainsFunc(ds, func(x cleaning.Decision) bool { return x.ID == d.ID }) {
		return nil, fmt.Errorf("decision %s is already recorded: a decision ID is the hash of its request, so this request was already applied; nothing was written", d.ID)
	}
	d = cloneDecision(d)
	if d.DecidedAt.IsZero() {
		d.DecidedAt = f.now()
	} else {
		d.DecidedAt = d.DecidedAt.Round(time.Microsecond).UTC()
	}
	return append(ds, d), nil
}

// ApplyRun writes w.Upserts against their expected states, then inserts
// w.Decisions, all or nothing.
func (f *Fake) ApplyRun(ctx context.Context, tenantID string, w cleaning.RunWrite) error {
	if err := checkTenant(ctx, tenantID); err != nil {
		return err
	}
	if f.ApplyRunFunc != nil {
		return f.ApplyRunFunc(ctx, tenantID, w)
	}
	for _, u := range w.Upserts {
		if err := u.Finding.Validate(); err != nil {
			return fmt.Errorf("apply run: %w", err)
		}
	}
	for _, d := range w.Decisions {
		if d.DecidedBy != cleaning.DecidedByPolicy {
			return fmt.Errorf("apply run: decision %s is decided by %q: ApplyRun records only policy decisions; record a reviewer's decision with Decide; nothing was written",
				d.ID, d.DecidedBy)
		}
		if err := d.Validate(); err != nil {
			return fmt.Errorf("apply run: %w", err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.load()
	found := maps.Clone(f.findings[tenantID])
	if found == nil {
		found = map[string]cleaning.StoredFinding{}
	}
	now := f.now()
	for _, u := range w.Upserts {
		existing, ok := found[u.Finding.ID]
		if ok != (u.ExpectState != "") || (ok && cleaning.StateToken(existing) != u.ExpectState) {
			return fmt.Errorf("apply run: %w", staleRun(u.Finding.ID))
		}
		s := cloneFinding(u.Finding)
		s.UpdatedAt = now
		found[s.ID] = s
	}
	ds := slices.Clone(f.decisions[tenantID])
	for _, d := range w.Decisions {
		var err error
		if ds, err = f.insertDecision(ds, found, d); err != nil {
			return fmt.Errorf("apply run: %w", err)
		}
	}
	f.findings[tenantID], f.decisions[tenantID] = found, ds
	return nil
}

// Decide records a reviewer's decision against an unchanged finding.
func (f *Fake) Decide(ctx context.Context, tenantID string, d cleaning.Decision, expectState string, next cleaning.StoredFinding) error {
	if err := checkTenant(ctx, tenantID); err != nil {
		return err
	}
	if f.DecideFunc != nil {
		return f.DecideFunc(ctx, tenantID, d, expectState, next)
	}
	if next.ID != d.FindingID {
		return fmt.Errorf("decide finding %s: next is finding %s; pass the decided finding's next state", d.FindingID, next.ID)
	}
	if d.DecidedBy == cleaning.DecidedByPolicy {
		return fmt.Errorf("decide finding %s: policy decisions are recorded only by ApplyRun; decide as a registered reviewer", d.FindingID)
	}
	if err := d.Validate(); err != nil {
		return fmt.Errorf("decide finding %s: %w", d.FindingID, err)
	}
	if err := next.Validate(); err != nil {
		return fmt.Errorf("decide finding %s: %w", d.FindingID, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.load()
	switch status, ok := f.Reviewers[tenantID][d.DecidedBy]; {
	case !ok:
		return fmt.Errorf("decide finding %s: reviewer %q: %w", d.FindingID, d.DecidedBy, cleaning.ErrReviewerNotRegistered)
	case status == ReviewerDeleted:
		return fmt.Errorf("decide finding %s: %w", d.FindingID, cleaning.ErrReviewerDeleted)
	case status != ReviewerActive:
		return fmt.Errorf("decide finding %s: Fake.Reviewers holds status %q for %q; use %q or %q",
			d.FindingID, status, d.DecidedBy, ReviewerActive, ReviewerDeleted)
	}
	current, ok := f.findings[tenantID][d.FindingID]
	if !ok {
		return fmt.Errorf("decide finding %s: %w", d.FindingID, cleaning.ErrFindingNotFound)
	}
	if cleaning.StateToken(current) != expectState {
		return fmt.Errorf("decide finding %s: %w", d.FindingID, cleaning.ErrStateChanged)
	}
	ds, err := f.insertDecision(slices.Clone(f.decisions[tenantID]), f.findings[tenantID], d)
	if err != nil {
		return fmt.Errorf("decide finding %s: %w", d.FindingID, err)
	}
	current.Status = next.Status
	current.Targets = slices.Clone(next.Targets)
	current.EdgeIDs = slices.Clone(next.EdgeIDs)
	current.GraphApplied = false
	current.UpdatedAt = f.now()
	f.findings[tenantID][d.FindingID] = current
	f.decisions[tenantID] = ds
	return nil
}

// Unprojected returns the findings with GraphApplied false, sorted by ID.
func (f *Fake) Unprojected(ctx context.Context, tenantID string) ([]cleaning.StoredFinding, error) {
	if err := checkTenant(ctx, tenantID); err != nil {
		return nil, err
	}
	if f.UnprojectedFunc != nil {
		return f.UnprojectedFunc(ctx, tenantID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.load()
	return f.sorted(tenantID, func(s cleaning.StoredFinding) bool { return !s.GraphApplied }), nil
}

// MarkProjected sets GraphApplied and replaces EdgeIDs.
func (f *Fake) MarkProjected(ctx context.Context, tenantID, findingID string, edgeIDs []string) error {
	if err := checkTenant(ctx, tenantID); err != nil {
		return err
	}
	if f.MarkProjectedFunc != nil {
		return f.MarkProjectedFunc(ctx, tenantID, findingID, edgeIDs)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.load()
	s, ok := f.findings[tenantID][findingID]
	if !ok {
		return fmt.Errorf("mark finding %s projected: %w", findingID, cleaning.ErrFindingNotFound)
	}
	s.GraphApplied, s.EdgeIDs, s.UpdatedAt = true, slices.Clone(edgeIDs), f.now()
	if err := s.Validate(); err != nil {
		return fmt.Errorf("mark finding %s projected: %w", findingID, err)
	}
	f.findings[tenantID][findingID] = s
	return nil
}

// PendingCount counts the tenant's findings with status pending.
func (f *Fake) PendingCount(ctx context.Context, tenantID string) (int64, error) {
	if err := checkTenant(ctx, tenantID); err != nil {
		return 0, err
	}
	if f.PendingCountFunc != nil {
		return f.PendingCountFunc(ctx, tenantID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.load()
	var n int64
	for _, s := range f.findings[tenantID] {
		if s.Status == cleaning.StatusPending {
			n++
		}
	}
	return n, nil
}

func cloneFinding(s cleaning.StoredFinding) cleaning.StoredFinding {
	s.Concerns = slices.Clone(s.Concerns)
	s.Targets = slices.Clone(s.Targets)
	s.EdgeIDs = slices.Clone(s.EdgeIDs)
	s.Evidence.Detail = maps.Clone(s.Evidence.Detail)
	if s.Evidence.Job != nil {
		job := *s.Evidence.Job
		s.Evidence.Job = &job
	}
	s.Evidence.Verdicts = slices.Clone(s.Evidence.Verdicts)
	for i, v := range s.Evidence.Verdicts {
		v.EdgeIDs = slices.Clone(v.EdgeIDs)
		if v.Agreement != nil {
			a := *v.Agreement
			v.Agreement = &a
		}
		s.Evidence.Verdicts[i] = v
	}
	return s
}

func cloneDecision(d cleaning.Decision) cleaning.Decision {
	d.Request = slices.Clone(d.Request)
	d.Attestation = slices.Clone(d.Attestation)
	return d
}
