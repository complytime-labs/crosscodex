package artifacts

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
)

const (
	controlLabel       = "Control"
	artifactGroupLabel = "ArtifactGroup"
	memberOfLabel      = "MEMBER_OF"
	sameAsLabel        = "SAME_AS"
	reconcilerName     = "artifact-reconciler"
	exactNameMethod    = "exact_name"
	tokenOverlapMethod = "token_overlap"
	reconcileThreshold = 0.6
)

// ReconcileAlgorithmVersion names the rule that produces SAME_AS edges:
// NormalizeArtifactName, TokenOverlap, the same-type gate and a threshold of
// 0.6. It is part of every SAME_AS edge ID, so change it whenever any of those
// changes; otherwise edges from two rules would share one edge set.
const ReconcileAlgorithmVersion = "token-overlap-v1"

// DefaultReconcileMaxEdges is the default ReconcileOptions.MaxEdges.
const DefaultReconcileMaxEdges = 50000

// ReconcileOptions configures one Reconcile run.
type ReconcileOptions struct {
	// DryRun computes and reports the run without writing to the graph.
	DryRun bool
	// MaxEdges is the most new SAME_AS edges one run may write. A run that
	// would write more fails before writing anything. Must be positive.
	MaxEdges int
	// ChunkSize is the most edges per BulkCreateEdges call. Must be positive.
	ChunkSize int
	// Now stamps valid_from and matched_at on everything the run writes.
	Now time.Time
}

// ReconcileResult reports one Reconcile run.
//
// In a real run, each New* field is what the run actually wrote: NewGroups
// counts only ArtifactGroup nodes CreateNode reported as newly created
// (ErrNodeExists does not count), and NewMemberships and NewMatches count
// edges writeEdges reported as created, which is partial when a write fails
// part-way. A step the run never reached reports 0.
//
// Under DryRun nothing is written, so each New* field is the planned count:
// what the run would write if it ran for real. NewGroups under DryRun is
// groups with no stored MEMBER_OF edge.
type ReconcileResult struct {
	Artifacts      int // current Artifact nodes grouped
	Skipped        int // current Artifact nodes without a usable name or type
	Groups         int // ArtifactGroups the artifacts form
	NewGroups      int // ArtifactGroup nodes created (under DryRun: groups with no stored MEMBER_OF edge)
	NewMemberships int // MEMBER_OF edges
	Matches        int // SAME_AS pairs among the groups
	NewMatches     int // SAME_AS edges
}

type artifactGroup struct {
	id, name, typ string
	members       []string // Artifact node IDs, sorted
}

type groupMatch struct {
	low, high string // group IDs, low < high
	score     float64
}

// Reconcile links equivalent Artifact nodes in tenantID's graph. Artifacts
// with the same type and normalized name join one ArtifactGroup node through
// MEMBER_OF edges; groups of one type whose names reach the TokenOverlap
// threshold get a SAME_AS edge from the lower group ID to the higher. No node
// is merged or deleted.
//
// Each run is a full pass and every ID is derived from content, so a run over
// an unchanged graph writes nothing. Each BulkCreateEdges chunk is one
// transaction; re-running completes a run that failed part-way. Run one
// Reconcile per tenant at a time: a concurrent run fails safely or, through
// the edge-write race documented on agedriver's checkEdgeWrite, duplicates an
// edge.
//
// The stored-edge queries cannot see superseded edges, so a superseded
// MEMBER_OF or SAME_AS edge keeps its ID forever: it is never recreated.
// Each run plans it again, then drops it on ErrEdgeExists; that costs an
// extra BulkCreateEdges attempt per such edge. It also inflates DryRun's New*
// counts (a group whose MEMBER_OF edges are all superseded counts in
// NewGroups), and a superseded SAME_AS edge counts against MaxEdges. Issue
// #170's adjudicator, which will supersede SAME_AS edges, must account for
// this.
func Reconcile(ctx context.Context, g graphdb.GraphDB, tenantID string, opts ReconcileOptions) (ReconcileResult, error) {
	var res ReconcileResult
	if opts.MaxEdges <= 0 || opts.ChunkSize <= 0 || opts.Now.IsZero() {
		return res, fmt.Errorf("reconcile artifacts: invalid options: MaxEdges (%d) and ChunkSize (%d) must be positive and Now must be set",
			opts.MaxEdges, opts.ChunkSize)
	}

	groups, loaded, skipped, err := loadGroups(ctx, g, tenantID)
	if err != nil {
		return res, fmt.Errorf("reconcile artifacts: load artifacts: %w", err)
	}
	matches := matchGroups(groups)
	res.Artifacts, res.Skipped, res.Groups, res.Matches = loaded, skipped, len(groups), len(matches)

	storedMembers, err := g.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{
		SourceLabel: artifactLabel, EdgeLabel: memberOfLabel, TargetLabel: artifactGroupLabel})
	if err != nil {
		return res, fmt.Errorf("reconcile artifacts: load MEMBER_OF edges: %w", err)
	}
	storedSameAs, err := g.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{
		SourceLabel: artifactGroupLabel, EdgeLabel: sameAsLabel, TargetLabel: artifactGroupLabel})
	if err != nil {
		return res, fmt.Errorf("reconcile artifacts: load SAME_AS edges: %w", err)
	}

	newGroups, memberships, sameAs := planWrites(groups, matches, storedMembers, storedSameAs, opts.Now)
	if len(sameAs) > opts.MaxEdges {
		return res, fmt.Errorf("reconcile artifacts: %d new SAME_AS edges exceed the limit of %d, so nothing was written; "+
			"look for an over-broad artifact name, or raise --max-edges", len(sameAs), opts.MaxEdges)
	}
	if opts.DryRun {
		res.NewGroups, res.NewMemberships, res.NewMatches = len(newGroups), len(memberships), len(sameAs)
		return res, nil
	}

	for _, n := range newGroups {
		// ErrNodeExists: an earlier run created the group but failed before
		// writing its MEMBER_OF edges; not counted, since this run created
		// nothing for it.
		if err = g.CreateNode(ctx, tenantID, n); err == nil {
			res.NewGroups++
			continue
		}
		if !errors.Is(err, graphdb.ErrNodeExists) {
			return res, fmt.Errorf("reconcile artifacts: create %s %s: %w", artifactGroupLabel, n.ID, err)
		}
	}
	if res.NewMemberships, err = writeEdges(ctx, g, tenantID, memberships, opts.ChunkSize); err != nil {
		return res, fmt.Errorf("reconcile artifacts: write MEMBER_OF edges: %w", err)
	}
	if res.NewMatches, err = writeEdges(ctx, g, tenantID, sameAs, opts.ChunkSize); err != nil {
		return res, fmt.Errorf("reconcile artifacts: write SAME_AS edges: %w", err)
	}
	return res, nil
}

// planWrites diffs groups and matches against the stored MEMBER_OF and
// SAME_AS edges and returns what a run must write to reach that state: the
// ArtifactGroup nodes with no stored MEMBER_OF edge, the MEMBER_OF edges
// current artifacts are missing, and the SAME_AS edges current matches are
// missing. now stamps ValidFrom and matched_at on everything it builds.
func planWrites(groups []*artifactGroup, matches []groupMatch, storedMembers, storedSameAs []graphdb.Relationship, now time.Time) (newGroups []graphdb.Node, memberships, sameAs []graphdb.BulkEdge) {
	storedEdges := make(map[string]bool, len(storedMembers)+len(storedSameAs))
	storedGroups := make(map[string]bool)
	for _, r := range storedMembers {
		storedEdges[r.Edge.ID] = true
		storedGroups[r.Target.ID] = true
	}
	for _, r := range storedSameAs {
		storedEdges[r.Edge.ID] = true
	}

	for _, gr := range groups {
		if !storedGroups[gr.id] {
			newGroups = append(newGroups, graphdb.Node{
				ID: gr.id, Label: artifactGroupLabel, ValidFrom: now,
				Properties: map[string]any{"name": gr.name, "type": gr.typ},
				CreatedBy:  reconcilerName, CreationMethod: exactNameMethod,
			})
		}
		for _, artID := range gr.members {
			id := graphdb.DerivedID("member-of", artID, gr.id)
			if storedEdges[id] {
				continue
			}
			memberships = append(memberships, graphdb.BulkEdge{SourceID: artID, TargetID: gr.id, Edge: graphdb.Edge{
				ID: id, Label: memberOfLabel, ValidFrom: now,
				DeterminedBy: reconcilerName, DeterminationType: exactNameMethod, Confidence: 1.0,
			}})
		}
	}
	for _, m := range matches {
		id := graphdb.DerivedID("same-as", ReconcileAlgorithmVersion, m.low, m.high)
		if storedEdges[id] {
			continue
		}
		sameAs = append(sameAs, graphdb.BulkEdge{SourceID: m.low, TargetID: m.high, Edge: graphdb.Edge{
			ID: id, Label: sameAsLabel, ValidFrom: now,
			DeterminedBy: reconcilerName, DeterminationType: tokenOverlapMethod, Confidence: m.score,
			Properties: map[string]any{
				"similarity_score":  m.score,
				"matched_at":        graphdb.FormatTime(now),
				"algorithm_version": ReconcileAlgorithmVersion,
			},
		}})
	}
	return newGroups, memberships, sameAs
}

// loadGroups reads every current Artifact node a Control DEMANDS and groups
// the artifacts by lowercased type and normalized name. It returns the groups
// sorted by ID, how many artifacts it grouped, and how many it skipped for
// lacking a usable name or type. QueryRelationships excludes superseded edges
// but not superseded nodes, so loadGroups drops those itself.
func loadGroups(ctx context.Context, g graphdb.GraphDB, tenantID string) ([]*artifactGroup, int, int, error) {
	rels, err := g.QueryRelationships(ctx, tenantID, graphdb.RelationshipQuery{
		SourceLabel: controlLabel, EdgeLabel: demandsLabel, TargetLabel: artifactLabel})
	if err != nil {
		return nil, 0, 0, err
	}
	byID := make(map[string]*artifactGroup)
	seen := make(map[string]bool, len(rels))
	loaded, skipped := 0, 0
	for _, r := range rels {
		art := r.Target
		if seen[art.ID] || art.ValidTo != nil {
			continue
		}
		seen[art.ID] = true
		name, _ := art.Properties["name"].(string)
		typ, _ := art.Properties["type"].(string)
		name = intanalyzer.NormalizeArtifactName(name)
		typ = strings.ToLower(strings.TrimSpace(typ))
		if name == "" || typ == "" {
			skipped++
			continue
		}
		id := graphdb.DerivedID("artifact-group", typ, name)
		gr, ok := byID[id]
		if !ok {
			gr = &artifactGroup{id: id, name: name, typ: typ}
			byID[id] = gr
		}
		gr.members = append(gr.members, art.ID)
		loaded++
	}
	groups := slices.SortedFunc(maps.Values(byID), func(a, b *artifactGroup) int { return cmp.Compare(a.id, b.id) })
	for _, gr := range groups {
		slices.Sort(gr.members)
	}
	return groups, loaded, skipped, nil
}

// matchGroups returns the SAME_AS pairs among groups, which must be sorted by
// ID: two groups of one type match when the TokenOverlap of their names
// reaches reconcileThreshold. An inverted token index limits scoring to pairs
// that share a token, the only pairs that can score above zero.
func matchGroups(groups []*artifactGroup) []groupMatch {
	type tokenKey struct{ typ, token string }
	index := make(map[tokenKey][]int)
	for i, gr := range groups {
		for _, tok := range strings.Fields(gr.name) {
			k := tokenKey{gr.typ, tok}
			if idx := index[k]; len(idx) == 0 || idx[len(idx)-1] != i {
				index[k] = append(idx, i)
			}
		}
	}
	var matches []groupMatch
	for i, gr := range groups {
		candidates := make(map[int]bool)
		for _, tok := range strings.Fields(gr.name) {
			for _, j := range index[tokenKey{gr.typ, tok}] {
				if j > i {
					candidates[j] = true
				}
			}
		}
		for _, j := range slices.Sorted(maps.Keys(candidates)) {
			if score := intanalyzer.TokenOverlap(gr.name, groups[j].name); score >= reconcileThreshold {
				matches = append(matches, groupMatch{low: gr.id, high: groups[j].id, score: score})
			}
		}
	}
	return matches
}

// writeEdges creates edges in BulkCreateEdges calls of at most size edges and
// returns how many it created. An edge reported as ErrEdgeExists is dropped
// and the rest of its chunk retried: the stored-edge queries cannot see a
// superseded edge or one a concurrent run just wrote, but either still owns
// its ID.
func writeEdges(ctx context.Context, g graphdb.GraphDB, tenantID string, edges []graphdb.BulkEdge, size int) (int, error) {
	created := 0
	for start := 0; start < len(edges); start += size {
		end := min(start+size, len(edges))
		chunk := slices.Clone(edges[start:end])
		for len(chunk) > 0 {
			_, err := g.BulkCreateEdges(ctx, tenantID, chunk)
			var be *graphdb.BulkEdgeError
			if errors.As(err, &be) && errors.Is(be.Err, graphdb.ErrEdgeExists) {
				chunk = slices.Delete(chunk, be.Index, be.Index+1)
				continue
			}
			if err != nil {
				return created, fmt.Errorf("edges %d-%d: %w", start, end-1, err)
			}
			created += len(chunk)
			break
		}
	}
	return created, nil
}
