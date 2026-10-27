package retentiontest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/retention"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ retention.HoldStore = (*Fake)(nil)

// Fake fakes retention.HoldStore. See the package documentation for the
// semantics it mirrors. The zero value is ready to use. Set the exported
// fields before the first call; the methods are then safe for concurrent use.
type Fake struct {
	// Seed holds are present before the first call, stored as given. Read once.
	Seed []retention.Hold
	// Now stamps CreatedAt and ReleasedAt. Nil means time.Now.
	Now func() time.Time

	// CreateFunc, if set, alone decides what Create returns; no hold is stored.
	CreateFunc func(ctx context.Context, h retention.Hold) (retention.Hold, error)
	// ReleaseFunc, if set, alone decides what Release returns; no hold changes.
	ReleaseFunc func(ctx context.Context, name, releasedBy string) error
	// ListFunc, if set, alone decides what List returns.
	ListFunc func(ctx context.Context) ([]retention.Hold, error)
	// ActiveFunc, if set, alone decides what Active returns.
	ActiveFunc func(ctx context.Context, now time.Time) ([]retention.Hold, error)

	mu      sync.Mutex
	loaded  bool
	holds   []retention.Hold // insertion order, never aliased outside the Fake
	created int              // highest sequence number Create has used for an ID
}

// tenantFor returns the ctx tenant, or the error the real store's tenant pool
// returns, worded like the store's own begin errors.
func tenantFor(ctx context.Context, op string) (string, error) {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return "", fmt.Errorf("hold store: begin %s: %w", op, db.ErrTenantRequired)
	}
	return tenantID, nil
}

// visible mirrors the retention_holds tenant_isolation USING clause.
func visible(h retention.Hold, tenantID string) bool {
	return h.Scope.TenantID == "" || h.Scope.TenantID == tenantID
}

// load copies Seed into the store on first use. Callers hold f.mu.
func (f *Fake) load() {
	if f.loaded {
		return
	}
	f.loaded = true
	for _, h := range f.Seed {
		f.holds = append(f.holds, cloneHold(h))
	}
}

func (f *Fake) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// Create stores a hold and returns it as the database would. See the package
// documentation.
func (f *Fake) Create(ctx context.Context, h retention.Hold) (retention.Hold, error) {
	tenantID, err := tenantFor(ctx, "create")
	if err != nil {
		return retention.Hold{}, err
	}
	if f.CreateFunc != nil {
		return f.CreateFunc(ctx, h)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.load()
	if h.Scope.TenantID != tenantID {
		return retention.Hold{}, errors.New("hold store: create: scope tenant is not the context tenant")
	}
	for _, existing := range f.holds {
		if existing.Name == h.Name && existing.ReleasedAt == nil {
			return retention.Hold{}, errors.New("hold store: create: an unreleased hold has this name")
		}
	}
	id := f.nextID()
	stored := retention.Hold{
		ID:        id,
		Name:      h.Name,
		Scope:     h.Scope,
		CreatedAt: f.now(),
		CreatedBy: h.CreatedBy,
		ExpiresAt: h.ExpiresAt,
	}
	f.holds = append(f.holds, cloneHold(stored))
	return cloneHold(stored), nil
}

// nextID returns the next "hold-N" not held by a stored hold, so a seeded hold
// can never share an ID with a created one. Callers hold f.mu.
func (f *Fake) nextID() string {
	for {
		f.created++
		id := fmt.Sprintf("hold-%d", f.created)
		if !slices.ContainsFunc(f.holds, func(h retention.Hold) bool { return h.ID == id }) {
			return id
		}
	}
}

// Release marks the visible unreleased holds named name as released. See the
// package documentation.
func (f *Fake) Release(ctx context.Context, name, releasedBy string) error {
	tenantID, err := tenantFor(ctx, "release")
	if err != nil {
		return err
	}
	if f.ReleaseFunc != nil {
		return f.ReleaseFunc(ctx, name, releasedBy)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.load()
	var targets []int
	for i, h := range f.holds {
		if h.Name != name || h.ReleasedAt != nil || !visible(h, tenantID) {
			continue
		}
		if h.Scope.TenantID == "" {
			// All-or-nothing, like the transaction: nothing is released.
			return errors.New("hold store: release: hold is global")
		}
		targets = append(targets, i)
	}
	releasedAt := f.now()
	for _, i := range targets {
		f.holds[i].ReleasedAt = &releasedAt
		f.holds[i].ReleasedBy = releasedBy
	}
	return nil
}

// List returns the visible holds, newest first, released ones included.
func (f *Fake) List(ctx context.Context) ([]retention.Hold, error) {
	tenantID, err := tenantFor(ctx, "list")
	if err != nil {
		return nil, err
	}
	if f.ListFunc != nil {
		return f.ListFunc(ctx)
	}
	return f.query(tenantID, func(retention.Hold) bool { return true }), nil
}

// Active returns the visible holds that are active at now, newest first.
func (f *Fake) Active(ctx context.Context, now time.Time) ([]retention.Hold, error) {
	tenantID, err := tenantFor(ctx, "active")
	if err != nil {
		return nil, err
	}
	if f.ActiveFunc != nil {
		return f.ActiveFunc(ctx, now)
	}
	return f.query(tenantID, func(h retention.Hold) bool { return h.ActiveAt(now) }), nil
}

// query returns deep copies of the visible holds that keep accepts, ordered
// newest CreatedAt first, ties most recently stored first. A result with no
// holds is nil, as scanHolds returns it.
func (f *Fake) query(tenantID string, keep func(retention.Hold) bool) []retention.Hold {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.load()
	var out []retention.Hold
	for _, h := range slices.Backward(f.holds) {
		if visible(h, tenantID) && keep(h) {
			out = append(out, cloneHold(h))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// cloneHold copies the pointer fields of a Hold, so neither a caller mutating
// a returned hold nor one mutating the hold it passed in alters the store.
func cloneHold(h retention.Hold) retention.Hold {
	h.Scope.CreatedBefore = cloneTime(h.Scope.CreatedBefore)
	h.ExpiresAt = cloneTime(h.ExpiresAt)
	h.ReleasedAt = cloneTime(h.ReleasedAt)
	return h
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}
