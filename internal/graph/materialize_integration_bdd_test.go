//go:build integration || integration_graph_nats

package graph_test

// Suite bootstrap lives in resolver_bdd_test.go — do NOT add RunSpecs here.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/graph"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
)

// crashOnEdge passes CreateEdge calls to the memdriver it embeds until the
// nth, which signals reached and blocks until release is closed, standing in
// for a graph role that dies mid-event. Every call after that fails without
// writing, as a dead process writes nothing more.
type crashOnEdge struct {
	graphdb.GraphDB
	n       int
	reached chan struct{}
	release chan struct{}

	mu      sync.Mutex
	calls   int
	crashed bool
}

func (g *crashOnEdge) CreateEdge(ctx context.Context, tenantID, sourceID, targetID string, edge graphdb.Edge) error {
	g.mu.Lock()
	g.calls++
	crash := g.calls == g.n
	dead := g.crashed
	if crash {
		g.crashed = true
	}
	g.mu.Unlock()
	switch {
	case crash:
		close(g.reached)
		<-g.release
		return errors.New("graph role crashed")
	case dead:
		return errors.New("graph role crashed")
	}
	return g.GraphDB.CreateEdge(ctx, tenantID, sourceID, targetID, edge)
}

// unstoredOutcomes passes calls to the natsbus.Client it embeds, except the
// first n durable publishes of a graph outcome, which fail as if AUDIT_EVENTS
// had not stored them.
type unstoredOutcomes struct {
	natsbus.Client
	mu sync.Mutex
	n  int
}

func (c *unstoredOutcomes) PublishDurable(ctx context.Context, subject string, data []byte, headers map[string][]string) error {
	c.mu.Lock()
	refuse := strings.HasSuffix(subject, ".graph."+natsbus.GraphOutcomeFailed) && c.n > 0
	if refuse {
		c.n--
	}
	c.mu.Unlock()
	if refuse {
		return fmt.Errorf("publish %s: %w", subject, natsbus.ErrNotStored)
	}
	return c.Client.PublishDurable(ctx, subject, data, headers)
}

var _ = Describe("Durable materialization on JetStream", func() {
	const (
		tenantID = "acme"
		jobID    = "job-1"
		subject  = "crosscodex.pipeline." + tenantID + "." + jobID + ".stage.completed"
	)

	var (
		url      string
		ctx      context.Context
		registry *graph.ResolverRegistry
		delivery config.NATSMaterializeConfig
	)

	newClient := func() natsbus.Client {
		c, err := natsbus.New(config.NATSConfig{
			URL:               url,
			PublishAckTimeout: 10 * time.Second,
			Streams: config.NATSStreamsConfig{
				AuditLLMRetention: 24 * time.Hour, AuditEventsRetention: 24 * time.Hour, MaterializeRetention: 24 * time.Hour,
			},
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = c.Close() })
		return c
	}

	publishStage := func(c natsbus.Client) {
		data, err := json.Marshal(map[string]string{"analyzer": "relationship", "job_id": jobID, "stage": "completed"})
		Expect(err).NotTo(HaveOccurred())
		Expect(c.PublishDurable(ctx, subject, data, nil)).To(Succeed())
	}

	queued := func(c natsbus.Client) func() (uint64, error) {
		return func() (uint64, error) { return c.SubjectMessageCount(ctx, natsbus.MaterializeStream, subject) }
	}

	BeforeEach(func() {
		srv, err := natsserver.NewServer(&natsserver.Options{
			Host: "127.0.0.1", Port: -1, JetStream: true,
			StoreDir: GinkgoT().TempDir(), NoLog: true, NoSigs: true,
		})
		Expect(err).NotTo(HaveOccurred())
		go srv.Start()
		DeferCleanup(srv.Shutdown)
		Expect(srv.ReadyForConnections(10 * time.Second)).To(BeTrue())
		url = srv.ClientURL()
		ctx = tenantCtx(tenantID)

		registry = graph.NewResolverRegistry()
		registry.Register(&mockResolver{scheme: "pg", data: []byte(`[
			{"source_id":"ctrl-1","target_id":"ctrl-2","relationship_type":"EQUIVALENT","confidence":0.9},
			{"source_id":"ctrl-2","target_id":"ctrl-3","relationship_type":"EQUIVALENT","confidence":0.9},
			{"source_id":"ctrl-1","target_id":"ctrl-3","relationship_type":"EQUIVALENT","confidence":0.9}]`)})
		delivery = config.NATSMaterializeConfig{MaxDeliver: 5, AckWait: time.Second, RedeliverDelay: 100 * time.Millisecond}
	})

	It("materializes the edges of an event whose consumer crashed before acking, exactly once", func() {
		mem := newMemGraph(ctx, tenantID, "ctrl-1", "ctrl-2", "ctrl-3")
		crashing := &crashOnEdge{GraphDB: mem, n: 2, reached: make(chan struct{}), release: make(chan struct{})}
		DeferCleanup(func() { close(crashing.release) })

		By("graph role A dying after writing the event's first edge, before it acks")
		clientA := newClient()
		roleA := graph.New(crashing, &mockVectorDB{}, clientA, graph.WithResolverRegistry(registry), graph.WithMaterializeConfig(delivery))
		Expect(roleA.Start(ctx)).To(Succeed())
		publishStage(clientA)
		Eventually(crashing.reached).WithTimeout(10 * time.Second).Should(BeClosed())
		Expect(clientA.Close()).To(Succeed())
		rels, err := mem.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: "SEMANTIC_MATCH"})
		Expect(err).NotTo(HaveOccurred())
		Expect(rels).To(HaveLen(1))

		By("graph role B, attached to the same durable consumer, getting the event after ack_wait")
		clientB := newClient()
		Expect(queued(clientB)()).To(Equal(uint64(1)), "the unacked event must still be in MATERIALIZE")
		roleB := graph.New(mem, &mockVectorDB{}, clientB, graph.WithResolverRegistry(registry), graph.WithMaterializeConfig(delivery))
		Expect(roleB.Start(ctx)).To(Succeed())
		DeferCleanup(func() { _ = roleB.Stop(context.Background()) })
		Eventually(queued(clientB)).WithTimeout(20*time.Second).WithPolling(100*time.Millisecond).Should(BeZero(),
			"role B must handle and ack the redelivered event")

		rels, err = mem.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: "SEMANTIC_MATCH"})
		Expect(err).NotTo(HaveOccurred())
		ids := map[string]int{}
		for _, r := range rels {
			ids[r.Edge.ID]++
		}
		Expect(ids).To(HaveLen(3))
		for id, n := range ids {
			Expect(n).To(Equal(1), "edge %s materialized %d times", id, n)
		}
	})

	It("redelivers an event past max_deliver until its failed outcome is stored, leaving MATERIALIZE empty", func() {
		delivery.MaxDeliver = 2
		c := newClient()
		filter, err := natsbus.GraphOutcomeFilter(tenantID, jobID)
		Expect(err).NotTo(HaveOccurred())
		outcomes := make(chan *natsbus.Message, 4)
		sub, err := c.Subscribe(ctx, filter, func(_ context.Context, msg *natsbus.Message) error {
			outcomes <- msg
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(sub.Unsubscribe)

		By("failing every graph write, and the failed outcome's first store on the last delivery")
		mem := newMemGraph(ctx, tenantID, "ctrl-1", "ctrl-2", "ctrl-3")
		role := graph.New(failingEdgeGraph{GraphDB: mem}, &mockVectorDB{}, &unstoredOutcomes{Client: c, n: 1},
			graph.WithResolverRegistry(registry), graph.WithMaterializeConfig(delivery))
		Expect(role.Start(ctx)).To(Succeed())
		DeferCleanup(func() { _ = role.Stop(context.Background()) })
		publishStage(c)

		var msg *natsbus.Message
		Eventually(outcomes).WithTimeout(20 * time.Second).Should(Receive(&msg))
		var got natsbus.GraphOutcome
		Expect(json.Unmarshal(msg.Data, &got)).To(Succeed())
		Expect(got.Outcome).To(Equal(natsbus.GraphOutcomeFailed))
		Expect(got.Analyzer).To(Equal("relationship"))
		Eventually(queued(c)).WithTimeout(10*time.Second).WithPolling(100*time.Millisecond).Should(BeZero(),
			"the event must be terminated once its failed outcome is stored, not stranded in MATERIALIZE")
	})

	It("answers the job's barrier with materialized once the job's events are handled, ignoring an earlier run's failure", func() {
		mem := newMemGraph(ctx, tenantID, "ctrl-1", "ctrl-2", "ctrl-3")
		c := newClient()

		By("storing a failed outcome from an earlier run of the same job ID")
		failedSubject, err := natsbus.GraphOutcomeSubject(tenantID, jobID, natsbus.GraphOutcomeFailed)
		Expect(err).NotTo(HaveOccurred())
		earlier, err := json.Marshal(natsbus.GraphOutcome{JobID: jobID, Outcome: natsbus.GraphOutcomeFailed, Analyzer: "relationship"})
		Expect(err).NotTo(HaveOccurred())
		Expect(c.PublishDurable(ctx, failedSubject, earlier, nil)).To(Succeed())
		runStart := time.Now()

		filter, err := natsbus.GraphOutcomeFilter(tenantID, jobID)
		Expect(err).NotTo(HaveOccurred())
		outcomes := make(chan *natsbus.Message, 4)
		sub, err := c.Subscribe(ctx, filter, func(_ context.Context, msg *natsbus.Message) error {
			outcomes <- msg
			return nil
		})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(sub.Unsubscribe)

		publishStage(c)
		barrierSubject, err := natsbus.MaterializeBarrierSubject(tenantID, jobID)
		Expect(err).NotTo(HaveOccurred())
		barrier, err := json.Marshal(natsbus.MaterializeBarrier{JobID: jobID, Deadline: time.Now().Add(time.Minute), NotBefore: runStart})
		Expect(err).NotTo(HaveOccurred())
		Expect(c.PublishDurable(ctx, barrierSubject, barrier, nil)).To(Succeed())

		role := graph.New(mem, &mockVectorDB{}, c, graph.WithResolverRegistry(registry), graph.WithMaterializeConfig(delivery))
		Expect(role.Start(ctx)).To(Succeed())
		DeferCleanup(func() { _ = role.Stop(context.Background()) })

		var msg *natsbus.Message
		Eventually(outcomes).WithTimeout(20 * time.Second).Should(Receive(&msg))
		var got natsbus.GraphOutcome
		Expect(json.Unmarshal(msg.Data, &got)).To(Succeed())
		Expect(got).To(Equal(natsbus.GraphOutcome{JobID: jobID, Outcome: natsbus.GraphOutcomeMaterialized}))
		Expect(queued(c)()).To(BeZero())
		rels, err := mem.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{EdgeLabel: "SEMANTIC_MATCH"})
		Expect(err).NotTo(HaveOccurred())
		Expect(rels).To(HaveLen(3), "materialized is only reported after the job's edges are written")
		Consistently(outcomes, 500*time.Millisecond).ShouldNot(Receive(), "the barrier is answered once")
	})
})
