package worker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/complytime-labs/crosscodex/pkg/natsbus"
)

// Memory per remembered abandon, on 64-bit Go. The key holds four cloned
// strings: a tenant ID (at most 52 bytes), a task type (at most 9), a job ID
// (at most natsbus.MaxJobIDLength, 128; the pipeline generates 36-byte
// UUIDs) and a collection UUID (36). natsbus.ParseAbandonedSubject rejects
// anything longer, so no publisher can make an entry bigger.
// With UUID job IDs the string data rounds up to allocator size classes of
// 64 + 16 + 48 + 48 = 176 bytes. The key itself is four 16-byte string
// headers, 64 bytes, stored twice (map key and FIFO slot) next to a 24-byte
// time.Time, so about 152 bytes plus map overhead (buckets run at most 81%
// full, with a tophash byte each) of about 40 bytes, and the FIFO slice can
// hold up to twice its live length after growth, another 64. Total: roughly
// 430 bytes, so about 4.3 MB at the default worker.max_abandoned_collections
// of 10000 and 43 MB at the maximum 100000. A job ID at the 128-byte cap
// takes the 128-byte size class instead of 48, adding 80 bytes: about
// 510 bytes per entry, 5.1 MB and 51 MB respectively.

// abandonedSet remembers recently abandoned collections so the worker can
// drop their queued tasks.
//
// Entries expire after ttl (pipeline.stage_timeout, see WithAbandonTTL).
// Every collection lives at most that long, so its tasks were dispatched at
// most that long before the abandon; a task still queued a full TTL after
// its abandon is rare and only costs one wasted LLM call. Expiry cannot
// cause a wrong drop: the key includes the collection ID, which no later
// collection reuses, so a long TTL only costs memory, which limit bounds.
//
// With one TTL for every entry, insertion order is also expiry order, so a
// FIFO queue serves both expiry (pop expired entries from the front) and the
// size bound (evict the oldest entry).
type abandonedSet struct {
	ttl   time.Duration
	limit int
	now   func() time.Time
	// onResize, if set, is called with the new size, under mu, whenever
	// the set grows or shrinks.
	onResize func(n int)

	mu      sync.Mutex
	expires map[natsbus.AbandonedCollection]time.Time
	order   []natsbus.AbandonedCollection // oldest first
}

func newAbandonedSet(ttl time.Duration, limit int, now func() time.Time) *abandonedSet {
	return &abandonedSet{
		ttl:     ttl,
		limit:   limit,
		now:     now,
		expires: make(map[natsbus.AbandonedCollection]time.Time),
	}
}

// add records c as abandoned and returns how many collections the set then
// holds. Re-adding a held collection keeps its original expiry; an expired
// one is removed first, so re-adding it starts a fresh TTL.
func (s *abandonedSet) add(c natsbus.AbandonedCollection) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := len(s.order)
	defer func() { s.reportResize(before) }()
	now := s.now()
	s.pruneExpired(now)
	if _, held := s.expires[c]; held {
		return len(s.order)
	}
	if len(s.order) >= s.limit {
		s.popOldest()
	}
	s.expires[c] = now.Add(s.ttl)
	s.order = append(s.order, c)
	return len(s.order)
}

// pruneExpired removes every expired entry. With one TTL they are all at the
// front. The caller holds mu.
func (s *abandonedSet) pruneExpired(now time.Time) {
	for len(s.order) > 0 && !now.Before(s.expires[s.order[0]]) {
		s.popOldest()
	}
}

// reportResize calls onResize when the size differs from before. The caller
// holds mu.
func (s *abandonedSet) reportResize(before int) {
	if s.onResize != nil && len(s.order) != before {
		s.onResize(len(s.order))
	}
}

// popOldest removes the oldest entry. The caller holds mu.
func (s *abandonedSet) popOldest() {
	delete(s.expires, s.order[0])
	s.order[0] = natsbus.AbandonedCollection{} // release its strings
	s.order = s.order[1:]
}

// contains reports whether c was abandoned less than ttl ago. It removes
// expired entries first, so the set's size, and the gauge fed by onResize,
// never counts collections it no longer acts on for longer than the gap
// between two tasks or abandons.
func (s *abandonedSet) contains(c natsbus.AbandonedCollection) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := len(s.order)
	s.pruneExpired(s.now())
	s.reportResize(before)
	_, held := s.expires[c]
	return held
}

// len returns how many collections the set holds, including any that expired
// since the last add or contains.
func (s *abandonedSet) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.order)
}

// dropAbandoned reports whether a task belongs to an abandoned collection
// and, if so, records the drop. It runs before the payload is decoded, so a
// dropped task gets no LLM call, no result and no progress tick; nobody is
// waiting for either. A task without a collection ID (published by something
// other than the analysis dispatcher) is never dropped. The log carries IDs
// only, never task content.
func (w *Worker) dropAbandoned(ctx context.Context, span trace.Span, tenantID, taskType, jobID, taskID, collectionID string) bool {
	if collectionID == "" {
		return false
	}
	c := natsbus.AbandonedCollection{
		TenantID: tenantID, TaskType: natsbus.TaskType(taskType), JobID: jobID, CollectionID: collectionID,
	}
	if !w.abandoned.contains(c) {
		return false
	}
	w.logger.Debug("dropped task of abandoned collection",
		"task_id", taskID,
		"task_type", taskType,
		"job_id", jobID,
		"collection_id", collectionID,
		"tenant_id", tenantID,
	)
	if w.droppedCounter != nil {
		w.droppedCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("reason", "abandoned"),
			attribute.String("task_type", taskType),
		))
	}
	span.SetAttributes(attribute.String("task.dropped", "abandoned"))
	span.SetStatus(codes.Ok, "dropped: collection abandoned")
	return true
}

// handleAbandoned records an abandon control message. The message comes off
// the bus, so it is validated here: the subject must parse, the provenance
// tenant must equal the subject's tenant, and it must carry no payload. A
// message that fails is dropped, logged and counted; nil is always returned,
// because a malformed message stays malformed on redelivery.
//
// The tenant comparison is a consistency check that rejects messages whose
// headers and subject disagree (a misbuilt publisher, a misrouted message).
// It is not authorization: provenance headers are unsigned, so any bus
// participant can set X-Tenant-Id to match whatever subject it publishes on.
// What keeps one party from abandoning another's collection is that the
// collection ID is a random UUID seen only in work message headers, and NATS
// publish permissions on crosscodex.control.>.
func (w *Worker) handleAbandoned(ctx context.Context, msg *natsbus.Message) error {
	c, err := natsbus.ParseAbandonedSubject(msg.Subject)
	if err != nil {
		w.rejectControl(ctx, err)
		return nil
	}
	if msg.Metadata.TenantID != c.TenantID {
		w.rejectControl(ctx, fmt.Errorf("provenance tenant %q is not the subject's tenant %q, so the message is inconsistent; publish abandons only for the publisher's own tenant",
			msg.Metadata.TenantID, c.TenantID))
		return nil
	}
	if len(msg.Data) != 0 {
		w.rejectControl(ctx, fmt.Errorf("message on %q has a %d-byte payload, but an abandon carries no payload", msg.Subject, len(msg.Data)))
		return nil
	}

	w.abandoned.add(c)
	w.logger.Debug("collection abandoned; dropping its tasks",
		"task_type", string(c.TaskType),
		"job_id", c.JobID,
		"collection_id", c.CollectionID,
		"tenant_id", c.TenantID,
	)
	return nil
}

// rejectControl logs and counts a dropped control message.
func (w *Worker) rejectControl(ctx context.Context, reason error) {
	w.logger.Warn("abandon control message dropped: the worker keeps running every task. "+
		"Abandons are published only by the analysis collector, through natsbus.AbandonedSubject",
		"reason", reason,
	)
	w.recordError(ctx, "unknown", "invalid_control_message")
}
