package analyzer

import (
	"context"
	"fmt"
	"sort"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// DAG is an immutable execution plan built from analyzer dependency
// declarations. It groups analyzers into levels for parallel execution
// and provides a flat topological order.
type DAG struct {
	levels [][]string
	order  []string
	edges  map[string][]string           // name -> dependencies
	nodes  map[string]RegisteredAnalyzer // name -> analyzer
}

// Levels returns a copy of the level-based execution groups.
// Analyzers within the same level can execute in parallel.
// Level 0 has no dependencies, level 1 depends only on level 0, etc.
func (d *DAG) Levels() [][]string {
	result := make([][]string, len(d.levels))
	for i, level := range d.levels {
		cp := make([]string, len(level))
		copy(cp, level)
		result[i] = cp
	}
	return result
}

// Order returns a copy of the flat topological order.
// Deterministic across calls (alphabetical within each level).
func (d *DAG) Order() []string {
	cp := make([]string, len(d.order))
	copy(cp, d.order)
	return cp
}

// Analyzers returns all analyzers in the DAG in topological order.
func (d *DAG) Analyzers() []RegisteredAnalyzer {
	result := make([]RegisteredAnalyzer, len(d.order))
	for i, name := range d.order {
		result[i] = d.nodes[name]
	}
	return result
}

// Subset builds a new DAG containing only the named analyzers and
// their transitive dependencies. Returns ErrNotFound if any name
// is not in the DAG.
func (d *DAG) Subset(names ...string) (*DAG, error) {
	// Validate all names exist.
	for _, name := range names {
		if _, ok := d.nodes[name]; !ok {
			return nil, fmt.Errorf("analyzer %q not found: %w", name, ErrNotFound)
		}
	}

	// Walk backwards to collect transitive dependencies.
	needed := make(map[string]bool)
	var walk func(name string)
	walk = func(name string) {
		if needed[name] {
			return
		}
		needed[name] = true
		for _, dep := range d.edges[name] {
			walk(dep)
		}
	}
	for _, name := range names {
		walk(name)
	}

	// Build subgraph with only the needed nodes.
	subNodes := make(map[string]RegisteredAnalyzer, len(needed))
	subEdges := make(map[string][]string, len(needed))
	for name := range needed {
		subNodes[name] = d.nodes[name]
		// Filter edges to only include dependencies that are in the subgraph.
		var deps []string
		for _, dep := range d.edges[name] {
			if needed[dep] {
				deps = append(deps, dep)
			}
		}
		subEdges[name] = deps
	}

	levels, order := kahnSort(subNodes, subEdges)

	return &DAG{
		levels: levels,
		order:  order,
		edges:  subEdges,
		nodes:  subNodes,
	}, nil
}

// BuildDAG constructs an immutable execution DAG from all registered
// analyzers. Uses Kahn's algorithm (BFS, in-degree tracking) to produce
// a topological sort grouped into levels.
//
// The ctx parameter is used for telemetry span parenting. Callers should
// pass context.Background() at startup or a request-scoped context when
// rebuilding the DAG dynamically.
//
// Returns ErrMissingDependency if any analyzer depends on an unregistered
// analyzer. Returns ErrCycleDetected if the dependency graph contains a cycle.
func (r *Registry) BuildDAG(ctx context.Context) (*DAG, error) {
	start := time.Now()

	r.mu.RLock()
	analyzers := make(map[string]RegisteredAnalyzer, len(r.analyzers))
	for k, v := range r.analyzers {
		analyzers[k] = v
	}
	r.mu.RUnlock()

	var span trace.Span
	if r.tracer != nil {
		_, span = r.tracer.Start(
			ctx,
			"analyzer.BuildDAG",
			trace.WithAttributes(
				attribute.Int("analyzer.count", len(analyzers)),
			),
		)
		defer span.End()
	}

	defer func() {
		if r.buildDAGLatency != nil {
			elapsed := float64(time.Since(start).Milliseconds())
			r.buildDAGLatency.Record(ctx, elapsed)
		}
	}()

	// Build adjacency list and validate dependencies.
	edges := make(map[string][]string, len(analyzers))
	for name, a := range analyzers {
		deps := a.DependsOn()
		for _, dep := range deps {
			if _, ok := analyzers[dep]; !ok {
				err := fmt.Errorf(
					"analyzer %q depends on %q which is not registered: %w",
					name, dep, ErrMissingDependency,
				)
				if span != nil {
					span.SetStatus(codes.Error, err.Error())
				}
				return nil, err
			}
		}
		edges[name] = deps
	}

	levels, order := kahnSort(analyzers, edges)

	// Cycle detection: if not all nodes were visited, cycles exist.
	if len(order) != len(analyzers) {
		cycle := findCycle(analyzers, edges, order)
		err := fmt.Errorf("%s: %w", cycle, ErrCycleDetected)
		if span != nil {
			span.SetStatus(codes.Error, err.Error())
		}
		return nil, err
	}

	if span != nil {
		span.SetAttributes(attribute.Int("dag.levels", len(levels)))
		span.SetStatus(codes.Ok, "")
	}

	return &DAG{
		levels: levels,
		order:  order,
		edges:  edges,
		nodes:  analyzers,
	}, nil
}

// kahnSort performs Kahn's algorithm with level-based grouping.
// Returns levels (groups of names that can run in parallel) and a flat
// topological order. Names within each level are sorted alphabetically.
func kahnSort(nodes map[string]RegisteredAnalyzer, edges map[string][]string) ([][]string, []string) {
	if len(nodes) == 0 {
		return nil, nil
	}

	// Compute in-degrees.
	inDegree := make(map[string]int, len(nodes))
	for name := range nodes {
		inDegree[name] = 0
	}
	// Each edge in `edges` is name -> [deps], meaning name depends on deps.
	// So deps are predecessors of name, and name is a successor of each dep.
	// In-degree of name = number of deps it has.
	for name, deps := range edges {
		inDegree[name] = len(deps)
	}

	// Collect initial nodes with in-degree 0.
	var queue []string
	for name, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, name)
		}
	}
	sort.Strings(queue)

	// Build reverse adjacency: dep -> [dependents].
	dependents := make(map[string][]string)
	for name, deps := range edges {
		for _, dep := range deps {
			dependents[dep] = append(dependents[dep], name)
		}
	}

	var levels [][]string
	var order []string

	for len(queue) > 0 {
		// Current queue is one level.
		level := make([]string, len(queue))
		copy(level, queue)
		levels = append(levels, level)
		order = append(order, level...)

		var next []string
		for _, name := range queue {
			for _, dependent := range dependents[name] {
				inDegree[dependent]--
				if inDegree[dependent] == 0 {
					next = append(next, dependent)
				}
			}
		}
		sort.Strings(next)
		queue = next
	}

	return levels, order
}

// findCycle extracts a representative cycle from the remaining unvisited
// nodes after Kahn's algorithm. visited is the set of nodes that were
// successfully sorted.
func findCycle(nodes map[string]RegisteredAnalyzer, edges map[string][]string, visited []string) string {
	visitedSet := make(map[string]bool, len(visited))
	for _, name := range visited {
		visitedSet[name] = true
	}

	// Find the remaining nodes (involved in cycles).
	var remaining []string
	for name := range nodes {
		if !visitedSet[name] {
			remaining = append(remaining, name)
		}
	}
	sort.Strings(remaining)

	if len(remaining) == 0 {
		return "dependency cycle detected (unknown nodes)"
	}

	// Walk from the first remaining node to find a cycle.
	start := remaining[0]
	path := []string{start}
	seen := map[string]int{start: 0} // name -> index in path

	current := start
	for {
		deps := make([]string, len(edges[current]))
		copy(deps, edges[current])
		sort.Strings(deps) // Deterministic cycle reporting.
		found := false
		for _, dep := range deps {
			if !visitedSet[dep] {
				if idx, ok := seen[dep]; ok {
					// Found the cycle.
					cycle := make([]string, len(path[idx:])+1)
					copy(cycle, path[idx:])
					cycle[len(cycle)-1] = dep
					return formatCycle(cycle)
				}
				seen[dep] = len(path)
				path = append(path, dep)
				current = dep
				found = true
				break
			}
		}
		if !found {
			// Dead end in remaining nodes — shouldn't happen in a cycle,
			// but handle gracefully.
			return formatCycle(remaining)
		}
	}
}

// formatCycle produces "a -> b -> c -> a" from a cycle slice.
func formatCycle(cycle []string) string {
	if len(cycle) == 0 {
		return "dependency cycle detected"
	}
	result := cycle[0]
	for i := 1; i < len(cycle); i++ {
		result += " -> " + cycle[i]
	}
	return result
}
