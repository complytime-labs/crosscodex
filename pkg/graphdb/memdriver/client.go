package memdriver

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

type memNode struct {
	label string
	props map[string]any
}

type memEdge struct {
	label  string
	props  map[string]any
	source *memNode
	target *memNode
	// seq stands in for AGE's internal graph ID, which agedriver returns as
	// the edge ID when the id property is empty.
	seq int64
}

type memGraph struct {
	nodes   []*memNode
	edges   []*memEdge
	lastSeq int64
}

// memClient holds one in-memory graph per tenant. A single RWMutex guards
// all of them: tests run NATS subscribers concurrently, and a coarse lock is
// cheaper than a flaky race.
type memClient struct {
	mu     sync.RWMutex
	graphs map[string]*memGraph
}

var _ graphdb.GraphDB = (*memClient)(nil)

// New returns an empty in-memory GraphDB with no tenant graphs.
func New() graphdb.GraphDB {
	return &memClient{graphs: make(map[string]*memGraph)}
}

// graph returns the tenant's graph. The caller must hold c.mu.
func (c *memClient) graph(tenant string) (*memGraph, error) {
	if err := graphdb.CheckTenant(tenant); err != nil {
		return nil, err
	}
	g, ok := c.graphs[tenant]
	if !ok {
		return nil, fmt.Errorf("graph for tenant %q does not exist; create the tenant (or call CreateGraph) before using its graph: %w", tenant, graphdb.ErrGraphNotFound)
	}
	return g, nil
}

// resolveNode returns the node whose id property equals id. CreateNode and
// UpsertNode keep IDs unique across labels, so at most one node matches.
func (g *memGraph) resolveNode(id string) (*memNode, error) {
	if n := g.nodeByID(id); n != nil {
		return n, nil
	}
	return nil, fmt.Errorf("node %q: %w", id, graphdb.ErrNodeNotFound)
}

// addEdge enforces the edge-creation contract (both endpoints resolve to
// exactly one node; a non-empty ID is unused) and appends the edge.
func (g *memGraph) addEdge(sourceID, targetID, label string, props map[string]any) error {
	src, err := g.resolveNode(sourceID)
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}
	tgt, err := g.resolveNode(targetID)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	if id := stringProp(props, "id"); id != "" {
		for _, e := range g.edges {
			if stringProp(e.props, "id") == id {
				return fmt.Errorf("edge %q: %w", id, graphdb.ErrEdgeExists)
			}
		}
	}
	g.lastSeq++
	g.edges = append(g.edges, &memEdge{label: label, props: props, source: src, target: tgt, seq: g.lastSeq})
	return nil
}

func decodeNode(n *memNode) graphdb.Node {
	props := copyProps(n.props)
	return graphdb.Node{
		ID:             stringProp(props, "id"),
		Label:          n.label,
		Properties:     props,
		ValidFrom:      timeProp(props, "valid_from"),
		ValidTo:        timePtrProp(props, "valid_to"),
		CreatedBy:      stringProp(props, "created_by"),
		CreationMethod: stringProp(props, "creation_method"),
	}
}

func decodeEdge(e *memEdge) graphdb.Edge {
	props := copyProps(e.props)
	edge := graphdb.Edge{
		ID:                stringProp(props, "id"),
		Label:             e.label,
		Properties:        props,
		ValidFrom:         timeProp(props, "valid_from"),
		ValidTo:           timePtrProp(props, "valid_to"),
		DeterminedBy:      stringProp(props, "determined_by"),
		DeterminationType: stringProp(props, "determination_type"),
		Confidence:        floatProp(props, "confidence"),
		Supersedes:        stringProp(props, "supersedes"),
	}
	if edge.ID == "" {
		edge.ID = strconv.FormatInt(e.seq, 10)
	}
	return edge
}

func (c *memClient) CreateGraph(_ context.Context, tenant string) error {
	if err := graphdb.CheckTenant(tenant); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.graphs[tenant]; !ok {
		c.graphs[tenant] = &memGraph{}
	}
	return nil
}

func (c *memClient) CreateNode(_ context.Context, tenant string, node graphdb.Node) error {
	if err := graphdb.CheckNode(node); err != nil {
		return fmt.Errorf("create node: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	g, err := c.graph(tenant)
	if err != nil {
		return err
	}
	if existing := g.nodeByID(node.ID); existing != nil {
		if existing.label != node.Label {
			return fmt.Errorf("create node: %w", &graphdb.NodeIDConflictError{ID: node.ID, Label: node.Label, StoredLabel: existing.label})
		}
		return fmt.Errorf("create node %s/%s: %w", node.Label, node.ID, graphdb.ErrNodeExists)
	}
	g.nodes = append(g.nodes, &memNode{label: node.Label, props: nodeProps(node)})
	return nil
}

func (c *memClient) UpsertNode(_ context.Context, tenant string, node graphdb.Node) (bool, error) {
	if err := graphdb.CheckNode(node); err != nil {
		return false, fmt.Errorf("upsert node: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	g, err := c.graph(tenant)
	if err != nil {
		return false, err
	}
	props := nodeProps(node)
	existing := g.nodeByID(node.ID)
	if existing == nil {
		g.nodes = append(g.nodes, &memNode{label: node.Label, props: props})
		return true, nil
	}
	if existing.label != node.Label {
		return false, fmt.Errorf("upsert node: %w", &graphdb.NodeIDConflictError{ID: node.ID, Label: node.Label, StoredLabel: existing.label})
	}
	// The stored valid_from is historical and always kept; valid_to and
	// superseded_by are SupersedeFact's unless the caller sets ValidTo. This
	// matches agedriver's SET n = {valid_from: n.valid_from, ...}.
	props["valid_from"] = existing.props["valid_from"]
	if node.ValidTo == nil {
		for _, k := range []string{"valid_to", "superseded_by"} {
			if v, ok := existing.props[k]; ok {
				props[k] = v
			}
		}
	}
	existing.props = props
	return false, nil
}

// nodeByID returns the node whose id property is id, or nil. IDs are unique
// across labels, so there is at most one.
func (g *memGraph) nodeByID(id string) *memNode {
	for _, n := range g.nodes {
		if stringProp(n.props, "id") == id {
			return n
		}
	}
	return nil
}

func (c *memClient) CreateEdge(_ context.Context, tenant, sourceID, targetID string, edge graphdb.Edge) error {
	if err := graphdb.CheckEdge(sourceID, targetID, edge); err != nil {
		return fmt.Errorf("create edge: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	g, err := c.graph(tenant)
	if err != nil {
		return err
	}
	if err := g.addEdge(sourceID, targetID, edge.Label, edgeProps(edge)); err != nil {
		return fmt.Errorf("create edge: %w", err)
	}
	return nil
}

func (c *memClient) CreateRequiresEdge(_ context.Context, tenant string, reqEdge graphdb.RequiresEdge) error {
	if err := graphdb.CheckRequiresEdge(reqEdge); err != nil {
		return fmt.Errorf("create requires edge: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	g, err := c.graph(tenant)
	if err != nil {
		return err
	}
	if err := g.addEdge(reqEdge.SourceID, reqEdge.TargetID, "REQUIRES", requiresProps(reqEdge)); err != nil {
		return fmt.Errorf("create requires edge: %w", err)
	}
	return nil
}

func (c *memClient) BulkCreateEdges(_ context.Context, tenant string, edges []graphdb.BulkEdge) ([]string, error) {
	// An empty batch returns nil, nil once the tenant is valid: it reads and
	// stores nothing, so it needs no graph. A malformed tenant is still
	// rejected (fail closed).
	if len(edges) == 0 {
		if err := graphdb.CheckTenant(tenant); err != nil {
			return nil, err
		}
		return nil, nil
	}
	for i, be := range edges {
		if err := graphdb.CheckEdge(be.SourceID, be.TargetID, be.Edge); err != nil {
			return nil, &graphdb.BulkEdgeError{Index: i, Err: err}
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	g, err := c.graph(tenant)
	if err != nil {
		return nil, err
	}
	edgeCount, lastSeq := len(g.edges), g.lastSeq
	ids := make([]string, 0, len(edges))
	for i, be := range edges {
		if err := g.addEdge(be.SourceID, be.TargetID, be.Edge.Label, edgeProps(be.Edge)); err != nil {
			clear(g.edges[edgeCount:])
			g.edges, g.lastSeq = g.edges[:edgeCount], lastSeq
			return nil, &graphdb.BulkEdgeError{Index: i, Err: err}
		}
		ids = append(ids, be.Edge.ID)
	}
	return ids, nil
}

func (c *memClient) QueryRelationships(_ context.Context, tenant string, query graphdb.RelationshipQuery) ([]graphdb.Relationship, error) {
	if err := graphdb.CheckRelationshipQuery(query); err != nil {
		return nil, fmt.Errorf("query relationships: %w", err)
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	g, err := c.graph(tenant)
	if err != nil {
		return nil, err
	}
	return g.relationships(query, nil), nil
}

func (c *memClient) QueryAsOf(_ context.Context, tenant string, query graphdb.RelationshipQuery, asOf time.Time) ([]graphdb.Relationship, error) {
	if err := graphdb.CheckRelationshipQuery(query); err != nil {
		return nil, fmt.Errorf("query relationships: %w", err)
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	g, err := c.graph(tenant)
	if err != nil {
		return nil, err
	}
	return g.relationships(query, &asOf), nil
}

// relationships returns edges matching q. With asOf nil it returns edges
// without valid_to; otherwise edges valid at asOf. Node validity is never
// checked, matching agedriver.
func (g *memGraph) relationships(q graphdb.RelationshipQuery, asOf *time.Time) []graphdb.Relationship {
	var out []graphdb.Relationship
	for _, e := range g.edges {
		if q.EdgeLabel != "" && e.label != q.EdgeLabel {
			continue
		}
		if q.SourceLabel != "" && e.source.label != q.SourceLabel {
			continue
		}
		if q.TargetLabel != "" && e.target.label != q.TargetLabel {
			continue
		}
		if !matchesProps(e.props, q.Properties) {
			continue
		}
		if asOf == nil {
			if _, superseded := e.props["valid_to"]; superseded {
				continue
			}
			out = append(out, graphdb.Relationship{Source: decodeNode(e.source), Edge: decodeEdge(e), Target: decodeNode(e.target)})
			continue
		}
		edge := decodeEdge(e)
		// A Path with only this edge checks just the edge's validity window;
		// reusing Path.ValidAt keeps one definition of that window.
		if !(graphdb.Path{Edges: []graphdb.Edge{edge}}).ValidAt(*asOf) {
			continue
		}
		out = append(out, graphdb.Relationship{Source: decodeNode(e.source), Edge: edge, Target: decodeNode(e.target)})
	}
	return out
}

func matchesProps(props, want map[string]any) bool {
	for k, v := range want {
		got, ok := props[k]
		if !ok || !reflect.DeepEqual(got, normalizeValue(v)) {
			return false
		}
	}
	return true
}

func (c *memClient) Traverse(_ context.Context, tenant string, query graphdb.TraversalQuery) ([]graphdb.Path, error) {
	for _, l := range query.EdgeLabels {
		if err := graphdb.CheckIdentifier("edge label", l); err != nil {
			return nil, fmt.Errorf("traverse: %w", err)
		}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	g, err := c.graph(tenant)
	if err != nil {
		return nil, err
	}
	var paths []graphdb.Path
	if start := g.nodeByID(query.StartNode); start != nil {
		g.walk(query, []*memNode{start}, nil, &paths)
	}
	if query.AsOf == nil {
		return paths, nil
	}
	var valid []graphdb.Path
	for _, p := range paths {
		if p.ValidAt(*query.AsOf) {
			valid = append(valid, p)
		}
	}
	return valid, nil
}

// walk records every one-hop extension of the path (nodes, edges) and
// recurses until MaxDepth. Like openCypher, a path never reuses an edge.
func (g *memGraph) walk(q graphdb.TraversalQuery, nodes []*memNode, edges []*memEdge, out *[]graphdb.Path) {
	if q.MaxDepth > 0 && len(edges) >= q.MaxDepth {
		return
	}
	current := nodes[len(nodes)-1]
	for _, e := range g.edges {
		if len(q.EdgeLabels) > 0 && !slices.Contains(q.EdgeLabels, e.label) {
			continue
		}
		if slices.Contains(edges, e) {
			continue
		}
		next := neighbor(e, current, q.Direction)
		if next == nil {
			continue
		}
		pathNodes := append(slices.Clone(nodes), next)
		pathEdges := append(slices.Clone(edges), e)
		*out = append(*out, decodePath(pathNodes, pathEdges))
		g.walk(q, pathNodes, pathEdges, out)
	}
}

// neighbor returns the node e leads to from `from` in the given direction,
// or nil when e is not traversable from `from`. Unknown directions mean
// outbound, matching agedriver.
func neighbor(e *memEdge, from *memNode, direction string) *memNode {
	switch direction {
	case "inbound":
		if e.target == from {
			return e.source
		}
	case "both":
		if e.source == from {
			return e.target
		}
		if e.target == from {
			return e.source
		}
	default:
		if e.source == from {
			return e.target
		}
	}
	return nil
}

func decodePath(nodes []*memNode, edges []*memEdge) graphdb.Path {
	p := graphdb.Path{
		Nodes: make([]graphdb.Node, len(nodes)),
		Edges: make([]graphdb.Edge, len(edges)),
	}
	for i, n := range nodes {
		p.Nodes[i] = decodeNode(n)
	}
	for i, e := range edges {
		p.Edges[i] = decodeEdge(e)
	}
	return p
}

func (c *memClient) GetNode(_ context.Context, tenant, nodeID string) (*graphdb.Node, error) {
	if nodeID == "" {
		return nil, fmt.Errorf("get node: node_id is required")
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	g, err := c.graph(tenant)
	if err != nil {
		return nil, err
	}
	if n := g.nodeByID(nodeID); n != nil {
		node := decodeNode(n)
		return &node, nil
	}
	return nil, fmt.Errorf("get node %s: %w", nodeID, graphdb.ErrNodeNotFound)
}

func (c *memClient) GetEdge(_ context.Context, tenant, edgeID string) (*graphdb.EdgeWithEndpoints, error) {
	if edgeID == "" {
		return nil, fmt.Errorf("get edge: edge_id is required")
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	g, err := c.graph(tenant)
	if err != nil {
		return nil, err
	}
	for _, e := range g.edges {
		if stringProp(e.props, "id") == edgeID {
			return &graphdb.EdgeWithEndpoints{
				Edge:     decodeEdge(e),
				SourceID: stringProp(e.source.props, "id"),
				TargetID: stringProp(e.target.props, "id"),
			}, nil
		}
	}
	return nil, fmt.Errorf("get edge %s: %w", edgeID, graphdb.ErrEdgeNotFound)
}

func (c *memClient) ExecuteQuery(_ context.Context, tenant, cypher string, _ map[string]string) ([]graphdb.QueryRow, error) {
	if cypher == "" {
		return nil, fmt.Errorf("execute query: cypher is required")
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if _, err := c.graph(tenant); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("execute query: memdriver has no openCypher engine; use the typed GraphDB methods, or run this test against agedriver in an integration suite: %w", graphdb.ErrNotSupported)
}

func (c *memClient) SupersedeFact(_ context.Context, tenant string, req graphdb.SupersedeRequest) (bool, error) {
	if err := graphdb.CheckSupersedeRequest(req); err != nil {
		return false, fmt.Errorf("supersede fact: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	g, err := c.graph(tenant)
	if err != nil {
		return false, err
	}

	var targets []map[string]any
	if req.NodeID != "" {
		if n := g.nodeByID(req.NodeID); n != nil {
			targets = append(targets, n.props)
		}
	} else {
		for _, e := range g.edges {
			if stringProp(e.props, "id") == req.EdgeID {
				targets = append(targets, e.props)
			}
		}
	}
	for _, props := range targets {
		props["valid_to"] = graphdb.FormatTime(req.SupersededAt)
		if req.SupersededByJobID != "" {
			props["superseded_by"] = req.SupersededByJobID
		}
	}
	return len(targets) > 0, nil
}
