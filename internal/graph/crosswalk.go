package graph

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/version"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/crosswalk"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	crosswalkDefaultPageSize = 50
	crosswalkMaxPageSize     = 1000

	// parentEdgeLabel is the catalog projection's parent-to-child edge.
	parentEdgeLabel = "PARENT_OF"

	noRelationshipMessage = "no-relationship verdicts are not stored: the relationship analyzer omits them, so a crosswalk lists related pairs only"
)

// pairKey identifies one (A control, B control) pair.
type pairKey struct{ a, b string }

// pairVerdicts collects the verdicts on one pair, with the endpoint nodes in
// the A-to-B reading.
type pairVerdicts struct {
	aNode, bNode graphdb.Node
	// verdicts holds every edge's verdict. A bidirectional job stores the pair
	// as two edges and contributes both; crosswalk.Merge counts the job once.
	verdicts []crosswalk.Verdict
}

// walkedRow is a crosswalk row plus the IDs of every job whose verdict it
// draws on; provenance job_ids is the union over the rows that survive the
// filters.
type walkedRow struct {
	row  *pb.CrosswalkRow
	jobs []string // distinct, sorted
}

// GetCrosswalk implements gateway.CrosswalkBackend for the job, pair,
// one_to_many and transitive modes:
// it walks the SEMANTIC_MATCH edges of the jobs the gateway selected, merges
// each pair's verdicts and returns one sorted page.
func (s *Service) GetCrosswalk(ctx context.Context, req *connect.Request[pb.GetCrosswalkRequest]) (*connect.Response[pb.GetCrosswalkResponse], error) {
	start := time.Now()
	ctx, span := s.startSpan(ctx, "graph.GetCrosswalk")
	defer span.End()

	msg := req.Msg
	fail := func(err error) (*connect.Response[pb.GetCrosswalkResponse], error) {
		s.recordRPC(ctx, "GetCrosswalk", start, connect.CodeOf(err))
		return nil, err
	}

	tenantID, err := s.extractTenant(ctx, msg.GetTenantContext())
	if err != nil {
		return fail(err)
	}

	var mode, a, b, hub string
	jobs := msg.GetJobs()
	switch m := msg.GetMode().(type) {
	case *pb.GetCrosswalkRequest_Pair:
		mode, a, b = "pair", m.Pair.GetCatalogA(), m.Pair.GetCatalogB()
		if a == "" || b == "" || a == b {
			return fail(connect.NewError(connect.CodeInvalidArgument, errors.New("pair mode needs two different catalogs: crosscodex crosswalk <a> <b>")))
		}
	case *pb.GetCrosswalkRequest_JobId:
		mode = "job"
		i := slices.IndexFunc(jobs, func(j *pb.CrosswalkJob) bool { return j.GetJobId() == m.JobId })
		if i < 0 {
			return fail(connect.NewError(connect.CodeNotFound, fmt.Errorf(
				"job %q not found: your tenant has no completed job with that ID. "+
					"Only completed jobs can be exported; check the job's status, or crosswalk its catalogs instead", m.JobId)))
		}
		jobs = jobs[i : i+1]
		a = jobs[0].GetSourceCatalogId()
		b = cmp.Or(jobs[0].GetTargetCatalogId(), a)
	case *pb.GetCrosswalkRequest_OneToManyCatalogId:
		mode, a = "one_to_many", m.OneToManyCatalogId
		if a == "" {
			return fail(connect.NewError(connect.CodeInvalidArgument, errors.New("one_to_many mode needs a catalog: crosscodex crosswalk <a> --all")))
		}
	case *pb.GetCrosswalkRequest_Transitive:
		mode, a, b, hub = "transitive", m.Transitive.GetCatalogA(), m.Transitive.GetCatalogC(), m.Transitive.GetHubCatalogId()
		if a == "" || b == "" || hub == "" || a == b || a == hub || b == hub {
			return fail(connect.NewError(connect.CodeInvalidArgument, errors.New("transitive mode needs three different catalogs (a, c and the hub): crosscodex crosswalk <a> <c> --via <hub>")))
		}
	default:
		return fail(connect.NewError(connect.CodeInvalidArgument, errors.New("no crosswalk mode set: name two catalogs or a job")))
	}
	span.SetAttributes(attribute.String("tenant.id", tenantID), attribute.String("crosswalk.mode", mode))

	if slices.Contains(msg.GetRelationshipTypes(), pb.RelationshipType_RELATIONSHIP_TYPE_NO_RELATIONSHIP) {
		return fail(connect.NewError(connect.CodeInvalidArgument, errors.New(noRelationshipMessage)))
	}
	pageSize, offset, tokenDigest, err := crosswalkPage(msg.GetPagination())
	if err != nil {
		return fail(err)
	}

	stage := results.StageSynthesized
	if msg.GetStage() == pb.CrosswalkStage_CROSSWALK_STAGE_ANALYZER {
		stage = results.StageAnalyzer
	}
	includeNonViable := msg.GetIncludeNonViable()
	digest := selectionDigest(msg, mode, a, b, hub, stage, jobs)
	if tokenDigest != "" && tokenDigest != digest {
		return fail(connect.NewError(connect.CodeInvalidArgument, errors.New(
			"the crosswalk changed while paging (a job completed or was removed): re-run the export to get a consistent result")))
	}
	var walked []walkedRow
	var skipped int64
	switch mode {
	case "one_to_many":
		// The gateway selected the completed jobs with a on either side; group
		// them by the catalog on the other side and walk each pair a to other.
		byOther := map[string][]*pb.CrosswalkJob{}
		for _, j := range jobs {
			other := j.GetTargetCatalogId()
			if other == a {
				other = j.GetSourceCatalogId()
			}
			byOther[other] = append(byOther[other], j)
		}
		for _, other := range slices.Sorted(maps.Keys(byOther)) {
			pairRows, pairSkipped, walkErr := s.walkPair(ctx, tenantID, a, other, byOther[other], stage, includeNonViable)
			if walkErr != nil {
				err = walkErr
				break
			}
			walked = append(walked, pairRows...)
			skipped += pairSkipped
		}
	case "transitive":
		walked, skipped, err = s.walkTransitive(ctx, tenantID, a, hub, b, jobs, stage, includeNonViable)
	default:
		walked, skipped, err = s.walkPair(ctx, tenantID, a, b, jobs, stage, includeNonViable)
	}
	if err != nil {
		code := mapGraphError(err)
		s.recordRPC(ctx, "GetCrosswalk", start, code)
		return nil, s.rpcError(ctx, "GetCrosswalk", code, err)
	}

	walked = filterCrosswalkRows(walked, msg.GetMinConfidence(), msg.GetRelationshipTypes())
	rows := make([]*pb.CrosswalkRow, len(walked))
	contributing := map[string]struct{}{}
	for i, w := range walked {
		rows[i] = w.row
		for _, id := range w.jobs {
			contributing[id] = struct{}{}
		}
	}
	for i, note := range crosswalk.EquivalenceNotes(rows) {
		// Collapse notes sit on derived rows and equivalence notes on direct
		// ones, so today at most one is non-empty; join keeps both if that changes.
		notes := slices.DeleteFunc([]string{rows[i].Review, note}, func(n string) bool { return n == "" })
		rows[i].Review = strings.Join(notes, "; ")
	}
	frameworks := map[string]string{}
	for _, c := range msg.GetCatalogs() {
		frameworks[c.GetCatalogId()] = c.GetName()
	}
	for _, r := range rows {
		for _, side := range []*pb.CrosswalkSide{r.Source, r.Target} {
			side.Framework = cmp.Or(frameworks[side.CatalogId], side.CatalogId)
		}
	}
	sortCrosswalkRows(rows)

	total := len(rows)
	if offset > total {
		return fail(connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
			"page_token points past the last row (offset %d of %d rows): use the next_page_token of a previous response for the same request, or omit it to start at the first page", offset, total)))
	}
	end := offset + min(pageSize, total-offset)
	page := rows[offset:end]
	if err := s.fillParents(ctx, tenantID, page); err != nil {
		code := mapGraphError(err)
		s.recordRPC(ctx, "GetCrosswalk", start, code)
		return nil, s.rpcError(ctx, "GetCrosswalk", code, err)
	}
	pageInfo := &pb.PageInfo{TotalCount: int64(total)}
	if end < total {
		pageInfo.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end) + ":" + digest))
	}
	selectedIDs := make([]string, len(jobs))
	for i, j := range jobs {
		selectedIDs[i] = j.GetJobId()
	}
	slices.Sort(selectedIDs)

	s.recordRPC(ctx, "GetCrosswalk", start, connect.Code(0))
	return connect.NewResponse(&pb.GetCrosswalkResponse{
		Rows:     page,
		PageInfo: pageInfo,
		Provenance: &pb.CrosswalkProvenance{
			Mode:                   mode,
			Stage:                  crosswalkStageEnum(stage),
			Catalogs:               msg.GetCatalogs(),
			JobIds:                 slices.Sorted(maps.Keys(contributing)),
			SelectedJobIds:         selectedIDs,
			Rows:                   int64(total),
			SkippedMissingControls: skipped,
			ServerVersion:          version.Version,
			GeneratedAt:            timestamppb.Now(),
		},
	}), nil
}

func crosswalkStageEnum(stage string) pb.CrosswalkStage {
	if stage == results.StageAnalyzer {
		return pb.CrosswalkStage_CROSSWALK_STAGE_ANALYZER
	}
	return pb.CrosswalkStage_CROSSWALK_STAGE_SYNTHESIZED
}

// selectionDigest fingerprints everything that decides which rows a crosswalk
// holds and in what order: the mode and its catalogs, the selected job IDs, the
// stage and the filters. A page token carries it, so a later page can tell that
// the selection changed under the offset.
func selectionDigest(msg *pb.GetCrosswalkRequest, mode, a, b, hub, stage string, jobs []*pb.CrosswalkJob) string {
	jobIDs := make([]string, len(jobs))
	for i, j := range jobs {
		jobIDs[i] = j.GetJobId()
	}
	slices.Sort(jobIDs)
	types := make([]string, len(msg.GetRelationshipTypes()))
	for i, t := range msg.GetRelationshipTypes() {
		types[i] = t.String()
	}
	slices.Sort(types)
	h := sha256.New()
	for _, field := range []string{
		mode, a, b, hub, stage,
		strconv.FormatBool(msg.GetIncludeNonViable()),
		strconv.FormatFloat(float64(msg.GetMinConfidence()), 'g', -1, 32),
		strings.Join(types, ","),
		strings.Join(jobIDs, ","),
	} {
		// A length prefix keeps adjacent fields from running together.
		fmt.Fprintf(h, "%d:%s;", len(field), field)
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// crosswalkPage validates pagination and decodes the opaque page token, which
// holds the offset of the next row and the digest of the selection that issued
// it.
func crosswalkPage(p *pb.Pagination) (size, offset int, digest string, err error) {
	invalid := func(format string, args ...any) error {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
	}
	size = int(p.GetPageSize())
	if size < 0 || size > crosswalkMaxPageSize {
		return 0, 0, "", invalid("page_size %d is out of range: use a value from 0 to %d (0 means the default)", size, crosswalkMaxPageSize)
	}
	if size == 0 {
		size = crosswalkDefaultPageSize
	}
	if tok := p.GetPageToken(); tok != "" {
		raw, decErr := base64.RawURLEncoding.DecodeString(tok)
		offsetText, digest, found := strings.Cut(string(raw), ":")
		offset, convErr := strconv.Atoi(offsetText)
		if decErr != nil || !found || digest == "" || convErr != nil || offset < 0 {
			return 0, 0, "", invalid("page_token is not valid: use the next_page_token of a previous response for the same request, or omit it to start at the first page")
		}
		return size, offset, digest, nil
	}
	return size, 0, "", nil
}

// walkPair returns the merged rows between catalogs a and b, read a to b, from
// the SEMANTIC_MATCH edges of stage written by the given jobs, and the number
// of edges skipped because an endpoint control is gone. With a == b (a job
// within one catalog) edges keep their stored direction. Framework names and
// parent identifiers are not filled: the caller owns the catalog names and
// calls fillParents on the rows it returns, so parents cost one lookup per
// returned control, not per walked control.
func (s *Service) walkPair(ctx context.Context, tenantID, a, b string, jobs []*pb.CrosswalkJob, stage string, includeNonViable bool) ([]walkedRow, int64, error) {
	jobByID := make(map[string]*pb.CrosswalkJob, len(jobs))
	for _, j := range jobs {
		jobByID[j.GetJobId()] = j
	}

	directions := []struct{ src, tgt string }{{a, b}}
	if a != b {
		directions = append(directions, struct{ src, tgt string }{b, a})
	}

	var skipped int64
	pairs := map[pairKey]*pairVerdicts{}
	for _, d := range directions {
		q := graphdb.RelationshipQuery{
			EdgeLabel:        results.SemanticMatchEdgeLabel,
			SourceProperties: map[string]any{"catalog_id": d.src},
			TargetProperties: map[string]any{"catalog_id": d.tgt},
		}
		// Edges written before the stage property existed are analyzer edges,
		// which an equality filter on the property cannot match.
		if stage == results.StageSynthesized {
			q.Properties = map[string]any{results.StageProperty: stage}
		}
		rels, err := s.graph.QueryRelationships(ctx, tenantID, q)
		if err != nil {
			return nil, 0, fmt.Errorf("query %s edges from %s to %s: %w", results.SemanticMatchEdgeLabel, d.src, d.tgt, err)
		}
		reversed := d.src != a
		for _, rel := range rels {
			job := jobByID[propString(rel.Edge.Properties, "job_id")]
			if job == nil || edgeStage(rel.Edge) != stage {
				continue
			}
			// Synthesis rejects some pairs but still stores them; like
			// results export, serve only the viable ones unless asked.
			if stage == results.StageSynthesized && !includeNonViable && !propBool(rel.Edge.Properties, "is_viable") {
				continue
			}
			typ := parseRelationshipType(propString(rel.Edge.Properties, "relationship_type"))
			if typ == pb.RelationshipType_RELATIONSHIP_TYPE_NO_RELATIONSHIP {
				continue
			}
			aNode, bNode := rel.Source, rel.Target
			if reversed {
				aNode, bNode = bNode, aNode
			}
			if !readableControl(aNode) || !readableControl(bNode) {
				skipped++
				continue
			}
			key := pairKey{aNode.ID, bNode.ID}
			pv := pairs[key]
			if pv == nil {
				pv = &pairVerdicts{aNode: aNode, bNode: bNode}
				pairs[key] = pv
			}
			var completedAt time.Time
			if t := job.GetCompletedAt(); t != nil {
				completedAt = t.AsTime()
			}
			pv.verdicts = append(pv.verdicts, crosswalk.Verdict{
				JobID:       job.GetJobId(),
				CompletedAt: completedAt,
				Type:        typ,
				Confidence:  rel.Edge.Confidence,
				Reversed:    reversed,
			})
		}
	}

	rows := make([]walkedRow, 0, len(pairs))
	for _, pv := range pairs {
		ids := make([]string, len(pv.verdicts))
		for i, v := range pv.verdicts {
			ids[i] = v.JobID
		}
		slices.Sort(ids)
		merged := crosswalk.Merge(pv.verdicts)
		word := crosswalk.Word(merged.Type)
		if merged.Direction == crosswalk.Reverse {
			word = "reverse: " + word
		}
		rows = append(rows, walkedRow{jobs: slices.Compact(ids), row: &pb.CrosswalkRow{
			Source:           crosswalkSide(pv.aNode),
			Target:           crosswalkSide(pv.bNode),
			RelationshipType: merged.Type,
			Relationship:     word,
			Confidence:       float32(merged.Confidence),
			Direction:        string(merged.Direction),
			JobsJudged:       int32(merged.JobsJudged),
			JobsAgreeing:     int32(merged.JobsAgreeing),
			OtherVerdicts:    crosswalk.FormatOthers(merged.Others),
			LatestJobId:      merged.LatestJob,
		}})
	}
	return rows, skipped, nil
}

// walkTransitive returns the direct rows between catalogs a and c plus one
// derived row per (a control, c control) pair: the a-to-hub and hub-to-c legs
// are each merged by walkPair, so a leg's row already holds the consensus of
// every job on that pair and a path is two hops by construction. Each path
// composes the leg types and multiplies the leg confidences; crosswalk.Collapse
// then folds a pair's paths into one verdict (see its doc for the rules) and
// the row names every hub control in HubIdentifier, "; "-joined. The row's jobs
// cover every leg of every path. OtherVerdicts stays empty on derived rows:
// the dissenting verdicts belong to the legs, not to the composed pair. A leg
// judged in the reverse direction composes as unknown; no-relationship legs
// start no path.
func (s *Service) walkTransitive(ctx context.Context, tenantID, a, hub, c string, jobs []*pb.CrosswalkJob, stage string, includeNonViable bool) ([]walkedRow, int64, error) {
	rows, skipped, err := s.walkPair(ctx, tenantID, a, c, jobs, stage, includeNonViable)
	if err != nil {
		return nil, 0, err
	}
	toHub, hubSkipped, err := s.walkPair(ctx, tenantID, a, hub, jobs, stage, includeNonViable)
	if err != nil {
		return nil, 0, err
	}
	fromHub, fromSkipped, err := s.walkPair(ctx, tenantID, hub, c, jobs, stage, includeNonViable)
	if err != nil {
		return nil, 0, err
	}
	skipped += hubSkipped + fromSkipped

	completedAt := make(map[string]time.Time, len(jobs))
	for _, j := range jobs {
		completedAt[j.GetJobId()] = j.GetCompletedAt().AsTime()
	}
	// newerJob picks the leg job that completed later; ties go to the greater
	// job ID, as crosswalk.Merge does.
	newerJob := func(x, y string) string {
		if cmp.Or(completedAt[x].Compare(completedAt[y]), strings.Compare(x, y)) >= 0 {
			return x
		}
		return y
	}
	// legType is the type to compose: a reverse-direction leg's type is not
	// known in the path's direction.
	legType := func(r *pb.CrosswalkRow) pb.RelationshipType {
		if r.Direction == string(crosswalk.Reverse) {
			return pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED
		}
		return r.RelationshipType
	}

	type derivedPair struct {
		source, target *pb.CrosswalkSide
		paths          []crosswalk.Path
	}
	// hubLabels maps a hub control's identifier to its published label.
	hubLabels := map[string]string{}
	var order []pairKey
	derived := map[pairKey]*derivedPair{}

	byHub := map[string][]walkedRow{}
	for _, r := range fromHub {
		byHub[r.row.Source.NodeId] = append(byHub[r.row.Source.NodeId], r)
	}
	for _, abw := range toHub {
		ab := abw.row
		hubLabels[ab.Target.Identifier] = ab.Target.Label
		for _, bcw := range byHub[ab.Target.NodeId] {
			bc := bcw.row
			typ, ok := crosswalk.Compose(legType(ab), legType(bc))
			if !ok {
				typ = pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED
			}
			latest := newerJob(ab.LatestJobId, bc.LatestJobId)
			key := pairKey{ab.Source.NodeId, bc.Target.NodeId}
			dp := derived[key]
			if dp == nil {
				dp = &derivedPair{source: ab.Source, target: bc.Target}
				derived[key] = dp
				order = append(order, key)
			}
			dp.paths = append(dp.paths, crosswalk.Path{
				Hub:        ab.Target.Identifier,
				Type:       typ,
				Confidence: float64(ab.Confidence) * float64(bc.Confidence),
				Jobs:       slices.Concat(abw.jobs, bcw.jobs),
				LatestJob:  latest,
				LatestAt:   completedAt[latest],
			})
		}
	}
	for _, key := range order {
		dp := derived[key]
		col := crosswalk.Collapse(dp.paths)
		word := "related (derived, type unknown)"
		if col.Type != pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED {
			word = crosswalk.Word(col.Type)
		}
		rows = append(rows, walkedRow{jobs: col.Jobs, row: &pb.CrosswalkRow{
			Source:           dp.source,
			Target:           dp.target,
			RelationshipType: col.Type,
			Relationship:     word,
			Confidence:       float32(col.Confidence),
			Direction:        string(crosswalk.Derived),
			JobsJudged:       int32(len(col.Jobs)),
			JobsAgreeing:     int32(col.JobsAgreeing),
			LatestJobId:      col.LatestJob,
			HubIdentifier:    strings.Join(col.Hubs, "; "),
			HubLabel:         strings.Join(hubLabelsOf(col.Hubs, hubLabels), "; "),
			PathsSupporting:  int32(col.PathsSupporting),
			PathsTotal:       int32(col.PathsTotal),
			Review:           strings.Join(col.Notes, "; "),
		}})
	}
	return rows, skipped, nil
}

// hubLabelsOf returns the label of each hub identifier, in order, so the
// result lines up with the identifiers; a hub without a label yields "".
func hubLabelsOf(hubs []string, labels map[string]string) []string {
	out := make([]string, len(hubs))
	for i, h := range hubs {
		out[i] = labels[h]
	}
	return out
}

// fillParents sets the parent identifier and the context breadcrumb of each
// side of rows from PARENT_OF. Each node's parent is looked up at most once per
// call, so siblings and shared ancestors cost no extra reads.
func (s *Service) fillParents(ctx context.Context, tenantID string, rows []*pb.CrosswalkRow) error {
	// parents maps a node ID to its chosen parent; nil means a top-level node.
	parents := map[string]*graphdb.Node{}
	parentOf := func(id string) (*graphdb.Node, error) {
		if p, ok := parents[id]; ok {
			return p, nil
		}
		paths, err := s.graph.Traverse(ctx, tenantID, graphdb.TraversalQuery{
			StartNode: id, Direction: "inbound", EdgeLabels: []string{parentEdgeLabel}, MaxDepth: 1,
		})
		if err != nil {
			return nil, err
		}
		p := lesserParent(paths)
		parents[id] = p
		return p, nil
	}
	type lineage struct{ parent, parentLabel, context string }
	seen := map[string]lineage{}
	for _, r := range rows {
		for _, side := range []*pb.CrosswalkSide{r.Source, r.Target} {
			l, ok := seen[side.NodeId]
			if !ok {
				chain, err := ancestorChain(side.NodeId, parentOf)
				if err != nil {
					return fmt.Errorf("find the ancestors of control %s: %w", side.NodeId, err)
				}
				l.parent, l.parentLabel, l.context = lineageOf(chain)
				seen[side.NodeId] = l
			}
			side.ParentIdentifier = l.parent
			side.ParentLabel = l.parentLabel
			side.Context = l.context
		}
	}
	return nil
}

// lesserParent returns the end node of the one-hop inbound path whose parent
// has the lesser node ID, or nil when there is none. Drivers do not order
// paths, so the ID tie-break keeps exports reproducible for a malformed
// catalog where a control has several parents.
func lesserParent(paths []graphdb.Path) *graphdb.Node {
	var best *graphdb.Node
	for _, p := range paths {
		if len(p.Nodes) < 2 {
			continue
		}
		if n := &p.Nodes[1]; best == nil || n.ID < best.ID {
			best = n
		}
	}
	return best
}

// ancestorChain walks PARENT_OF upward from id one hop at a time, nearest
// ancestor first. It follows the parent with the lesser node ID at each hop and
// stops at a node without a parent or at the first node already on the chain,
// so a cyclic catalog terminates. There is no depth limit: the data decides.
func ancestorChain(id string, parentOf func(string) (*graphdb.Node, error)) ([]graphdb.Node, error) {
	visited := map[string]bool{id: true}
	var chain []graphdb.Node
	for cur := id; ; {
		p, err := parentOf(cur)
		if err != nil {
			return nil, err
		}
		if p == nil || visited[p.ID] {
			return chain, nil
		}
		visited[p.ID] = true
		chain = append(chain, *p)
		cur = p.ID
	}
}

// lineageOf reads the direct parent's identifier and label and the breadcrumb
// of titled ancestors from an ancestor chain, nearest first. Each part is
// crosswalk.LineageSegment of the ancestor (title with its label, else its
// identifier, else the title alone), root first, joined " > "; untitled
// ancestors are omitted.
func lineageOf(chain []graphdb.Node) (parent, parentLabel, breadcrumb string) {
	if len(chain) == 0 {
		return "", "", ""
	}
	parent = propString(chain[0].Properties, "identifier")
	parentLabel = propString(chain[0].Properties, "label")
	var parts []string
	for _, n := range chain {
		if seg := crosswalk.LineageSegment(propString(n.Properties, "title"), propString(n.Properties, "label"), propString(n.Properties, "identifier")); seg != "" {
			parts = append(parts, seg)
		}
	}
	slices.Reverse(parts)
	return parent, parentLabel, strings.Join(parts, " > ")
}

// edgeStage reads an edge's stage; an edge without the property is an
// analyzer edge.
func edgeStage(e graphdb.Edge) string {
	if st := propString(e.Properties, results.StageProperty); st != "" {
		return st
	}
	return results.StageAnalyzer
}

// readableControl reports whether a node carries what a crosswalk row needs: an
// ID and the control's published identifier. A node without them belongs to a
// catalog that was re-imported or emptied after the edge was written.
func readableControl(n graphdb.Node) bool {
	return n.ID != "" && propString(n.Properties, "identifier") != ""
}

func crosswalkSide(n graphdb.Node) *pb.CrosswalkSide {
	return &pb.CrosswalkSide{
		NodeId:     n.ID,
		CatalogId:  propString(n.Properties, "catalog_id"),
		Identifier: propString(n.Properties, "identifier"),
		Label:      propString(n.Properties, "label"),
		Title:      propString(n.Properties, "title"),
		Statement:  propString(n.Properties, "statement"),
	}
}

// parseRelationshipType reads a stored relationship type, either the bare name
// ("SUBSET_OF") or the proto enum name. An unknown name yields UNSPECIFIED, so
// the row is reported rather than dropped.
func parseRelationshipType(name string) pb.RelationshipType {
	if v, ok := pb.RelationshipType_value["RELATIONSHIP_TYPE_"+name]; ok {
		return pb.RelationshipType(v)
	}
	if v, ok := pb.RelationshipType_value[name]; ok {
		return pb.RelationshipType(v)
	}
	return pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED
}

func propBool(props map[string]any, key string) bool {
	b, _ := props[key].(bool)
	return b
}

func propString(props map[string]any, key string) string {
	s, _ := props[key].(string)
	return s
}

// filterCrosswalkRows applies the post-merge filters: rows below minConfidence
// and, when types is non-empty, rows of other types are dropped.
func filterCrosswalkRows(rows []walkedRow, minConfidence float32, types []pb.RelationshipType) []walkedRow {
	return slices.DeleteFunc(rows, func(w walkedRow) bool {
		r := w.row
		return r.Confidence < minConfidence || (len(types) > 0 && !slices.Contains(types, r.RelationshipType))
	})
}

// sortCrosswalkRows orders rows by target framework (constant within a pair),
// source identifier, the pair's best confidence (descending), then target
// identifier and node IDs, which keeps each (source, target) pair contiguous.
// A pair has at most one direct and one derived row (transitive mode), the
// direct row first, so an inferred row never reads as the pair's main answer.
// Outside transitive mode a pair has one row, so its best confidence is its
// own and the order is by confidence alone.
func sortCrosswalkRows(rows []*pb.CrosswalkRow) {
	type pair struct{ src, tgt string }
	best := map[pair]float32{}
	for _, r := range rows {
		k := pair{r.Source.NodeId, r.Target.NodeId}
		best[k] = max(best[k], r.Confidence)
	}
	slices.SortFunc(rows, func(x, y *pb.CrosswalkRow) int {
		return cmp.Or(
			cmp.Compare(x.Target.Framework, y.Target.Framework),
			cmp.Compare(x.Source.Identifier, y.Source.Identifier),
			cmp.Compare(best[pair{y.Source.NodeId, y.Target.NodeId}], best[pair{x.Source.NodeId, x.Target.NodeId}]),
			cmp.Compare(x.Target.Identifier, y.Target.Identifier),
			cmp.Compare(x.Source.NodeId, y.Source.NodeId),
			cmp.Compare(x.Target.NodeId, y.Target.NodeId),
			cmp.Compare(derivedRank(x), derivedRank(y)),
			cmp.Compare(y.Confidence, x.Confidence),
		)
	})
}

func derivedRank(r *pb.CrosswalkRow) int {
	if r.Direction == string(crosswalk.Derived) {
		return 1
	}
	return 0
}
