package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

const (
	artifactLabel     = "Artifact"
	artifactTypeLabel = "ArtifactType"
	demandsLabel      = "DEMANDS"
	isTypeLabel       = "IS_TYPE"
	determinationType = "llm_panel"
	storagePrefix     = "analysis/artifacts/"
)

// GraphMaterializer creates graph nodes and edges from stored control results.
// Nothing in production calls it yet. The live writer is internal/graph
// (materializeArtifacts in subscriber_handlers.go), which writes artifact and
// artifact-type nodes under different IDs and without the content check
// Materialize does (see "Graph writers" in docs/dev/design-principles.md).
type GraphMaterializer struct {
	graph  graphdb.GraphDB
	store  storage.Provider
	cfg    config.ArtifactsConfig
	tracer trace.Tracer

	nodeCounter metric.Int64Counter
	edgeCounter metric.Int64Counter
}

// NewGraphMaterializer creates a materializer with the given dependencies.
func NewGraphMaterializer(graph graphdb.GraphDB, store storage.Provider, cfg config.ArtifactsConfig, opts ...MaterializerOption) *GraphMaterializer {
	m := &GraphMaterializer{
		graph: graph,
		store: store,
		cfg:   cfg,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// MaterializerOption configures the GraphMaterializer.
type MaterializerOption func(*GraphMaterializer)

// WithMaterializerTelemetry enables tracing and metrics for the materializer.
func WithMaterializerTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) MaterializerOption {
	return func(m *GraphMaterializer) {
		if tp != nil {
			m.tracer = tp.Tracer("crosscodex/internal/analyzer/artifacts")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			m.nodeCounter, _ = meter.Int64Counter(
				"artifacts.nodes.materialized",
				metric.WithDescription("Total artifact graph nodes materialized"),
			)
			m.edgeCounter, _ = meter.Int64Counter(
				"artifacts.edges.materialized",
				metric.WithDescription("Total artifact graph edges materialized"),
			)
		}
	}
}

// Materialize reads all control results for a job from object storage and
// creates corresponding graph nodes and edges.
//
// Re-running Materialize for the same job into the same graph is safe and
// resumes a partial earlier run: existing ArtifactType nodes and edges are
// skipped, and an existing Artifact node is accepted if it holds the same
// content this run would write, whichever job wrote it (the stored node keeps
// the first job's created_by). Artifact IDs do not include the job, so an
// artifact whose content differs from a stored node, or from another artifact
// in the same job with the same ID, makes Materialize return an error wrapping
// graphdb.ErrNodeExists. A referenced control missing from the graph returns
// an error wrapping graphdb.ErrNodeNotFound, and an unknown artifact type is
// rejected. All of these are checked before the first write, so a failing job
// writes nothing, except that the graph has no cross-call transaction: a
// concurrent writer can still create a conflicting node between the check and
// the write.
func (m *GraphMaterializer) Materialize(ctx context.Context, tenantID, jobID string) error {
	if err := tenant.ValidateTenantID(tenantID); err != nil {
		return fmt.Errorf("materializer.Materialize: %w", err)
	}

	ctx, span := m.startSpan(ctx, "materializer.Materialize")
	defer span.End()

	span.SetAttributes(
		attribute.String("tenant.id", tenantID),
		attribute.String("job_id", jobID),
	)

	fail := func(err error) error {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	// Step 1: Read every control result before writing anything.
	prefix := tenantID + "/" + storagePrefix + jobID + "/"
	objects, err := m.store.List(ctx, prefix)
	if err != nil {
		return fail(fmt.Errorf("materializer.Materialize: listing results: %w", err))
	}

	var planned []plannedArtifact
	for _, obj := range objects {
		reader, err := m.store.Get(ctx, obj.Key)
		if err != nil {
			return fail(fmt.Errorf("materializer.Materialize: reading %s: %w", obj.Key, err))
		}

		data, err := io.ReadAll(reader)
		if closeErr := reader.Close(); closeErr != nil {
			slog.WarnContext(ctx, "materializer.Materialize: closing reader",
				"key", obj.Key, "error", closeErr)
		}
		if err != nil {
			return fail(fmt.Errorf("materializer.Materialize: reading body %s: %w", obj.Key, err))
		}

		var cr ControlResult
		if err := json.Unmarshal(data, &cr); err != nil {
			return fail(fmt.Errorf("materializer.Materialize: parsing %s: %w", obj.Key, err))
		}

		for _, art := range cr.Artifacts {
			planned = append(planned, plannedArtifact{
				controlID: cr.ControlID,
				art:       art,
				node:      artifactNode(jobID, cr.ControlID, art),
			})
		}
	}

	// Step 2: Reject the whole job if any artifact conflicts.
	verified, err := m.checkArtifacts(ctx, tenantID, planned)
	if err != nil {
		return fail(err)
	}

	// Step 3: Create static ArtifactType nodes, then the artifacts.
	if err := m.ensureArtifactTypeNodes(ctx, tenantID); err != nil {
		return fail(err)
	}
	for _, p := range planned {
		if err := m.materializeArtifact(ctx, tenantID, jobID, p, verified[p.node.ID]); err != nil {
			return fail(err)
		}
	}

	span.SetAttributes(
		attribute.Int("control.count", len(objects)),
		attribute.Int("artifact.count", len(planned)),
	)
	span.SetStatus(codes.Ok, "")
	return nil
}

// plannedArtifact is one artifact of a job, with the node Materialize will
// write for it.
type plannedArtifact struct {
	controlID string
	art       ConsensusArtifact
	node      graphdb.Node
}

func (m *GraphMaterializer) ensureArtifactTypeNodes(ctx context.Context, tenantID string) error {
	now := time.Now()
	for _, at := range AllArtifactTypes() {
		id := strings.ToLower(at.String())
		node := graphdb.Node{
			ID:             id,
			Label:          artifactTypeLabel,
			Properties:     map[string]any{"name": at.TitleCase()},
			ValidFrom:      now,
			CreatedBy:      "system",
			CreationMethod: "static",
		}
		err := m.graph.CreateNode(ctx, tenantID, node)
		if err != nil {
			if errors.Is(err, graphdb.ErrNodeExists) {
				continue
			}
			return fmt.Errorf("materializer: creating ArtifactType %s: %w", id, err)
		}
		if m.nodeCounter != nil {
			m.nodeCounter.Add(ctx, 1, metric.WithAttributes(
				attribute.String("node.label", artifactTypeLabel),
			))
		}
	}
	return nil
}

// artifactID generates a deterministic ID from control ID and artifact content.
func artifactID(controlID string, art ConsensusArtifact) string {
	normalized := intanalyzer.NormalizeArtifactName(art.Name)
	input := normalized + ":" + art.Type.String()
	hash := sha256.Sum256([]byte(input))
	return fmt.Sprintf("%s__art_%x", controlID, hash[:4])
}

// artifactNode builds the Artifact node a job writes for art.
func artifactNode(jobID, controlID string, art ConsensusArtifact) graphdb.Node {
	props := map[string]any{
		"name":        art.Name,
		"frequency":   art.Frequency,
		"owner_role":  art.OwnerRole,
		"description": art.Description,
		"confidence":  art.Confidence,
	}
	for k, v := range art.Properties {
		props["prop_"+k] = v
	}

	return graphdb.Node{
		ID:             artifactID(controlID, art),
		Label:          artifactLabel,
		Properties:     props,
		ValidFrom:      time.Now(),
		CreatedBy:      jobID,
		CreationMethod: determinationType,
	}
}

// checkArtifacts verifies, before anything is written, that every artifact
// has a known type, every control the job references exists, and no planned
// artifact conflicts with a stored node or with another artifact in the same
// job, so a failing job writes nothing. It returns the IDs of planned
// artifacts it found already stored and matching. The graph shares no
// transaction across calls, so a concurrent writer can still create a
// conflicting node between this check and the write; materializeArtifact
// re-checks on ErrNodeExists for the IDs this check found absent to catch
// that.
func (m *GraphMaterializer) checkArtifacts(ctx context.Context, tenantID string, planned []plannedArtifact) (map[string]bool, error) {
	inJob := make(map[string]graphdb.Node, len(planned))
	controls := make(map[string]bool)
	verified := make(map[string]bool)
	for _, p := range planned {
		if !p.art.Type.Valid() {
			return nil, fmt.Errorf("materializer: cannot materialize artifact %q of control %s for job %q: artifact type "+
				"%d is not a known ArtifactType, so nothing was written. Fix the analyzer output for this job, then "+
				"re-run it", p.art.Name, p.controlID, p.node.CreatedBy, int(p.art.Type))
		}
		if !controls[p.controlID] {
			controls[p.controlID] = true
			if _, err := m.graph.GetNode(ctx, tenantID, p.controlID); err != nil {
				if errors.Is(err, graphdb.ErrNodeNotFound) {
					return nil, fmt.Errorf("materializer: cannot materialize artifacts for control %s: the control does not "+
						"exist in the tenant graph, so nothing was written. Catalog ingest creates control nodes; "+
						"import the catalog first, then re-run this job: %w", p.controlID, err)
				}
				return nil, fmt.Errorf("materializer: checking control %s exists "+
					"(retry once the graph is reachable; re-running the job is safe): %w", p.controlID, err)
			}
		}
		if prev, ok := inJob[p.node.ID]; ok {
			if differing := differingFields(prev, p.node); len(differing) > 0 {
				return nil, fmt.Errorf("materializer: cannot materialize Artifact %s for job %q: artifacts %q and %q of "+
					"control %s normalize to the same ID but differ in %s, so nothing was written. This is a bug "+
					"in the analyzer output; fix it before re-running the job: %w",
					p.node.ID, p.node.CreatedBy, prev.Properties["name"], p.art.Name, p.controlID,
					strings.Join(differing, ", "), graphdb.ErrNodeExists)
			}
			continue
		}
		inJob[p.node.ID] = p.node
		found, err := m.checkStoredArtifact(ctx, tenantID, p.node)
		if err != nil {
			return nil, err
		}
		if found {
			verified[p.node.ID] = true
		}
	}
	return verified, nil
}

// materializeArtifact writes p's Artifact node and its DEMANDS and IS_TYPE
// edges. verified reports that checkArtifacts already found the node stored
// and matching; this materializer never replaces a stored Artifact, so the
// node it collides with is the one already compared and needs no second
// lookup. A node checkArtifacts found absent must have been written
// concurrently, so it is compared now.
func (m *GraphMaterializer) materializeArtifact(ctx context.Context, tenantID, jobID string, p plannedArtifact, verified bool) error {
	controlID, art, node := p.controlID, p.art, p.node
	now, artID := node.ValidFrom, node.ID

	err := m.graph.CreateNode(ctx, tenantID, node)
	switch {
	case errors.Is(err, graphdb.ErrNodeExists):
		if !verified {
			if _, err := m.checkStoredArtifact(ctx, tenantID, node); err != nil {
				return err
			}
		}
	case err != nil:
		return fmt.Errorf("materializer: creating Artifact %s: %w", artID, err)
	case m.nodeCounter != nil:
		m.nodeCounter.Add(ctx, 1, metric.WithAttributes(
			attribute.String("node.label", artifactLabel),
		))
	}

	// Create DEMANDS edge: (Control) -> (Artifact).
	demandsEdge := graphdb.Edge{
		ID:                graphdb.DerivedID("demands", controlID, artID),
		Label:             demandsLabel,
		DeterminedBy:      jobID,
		DeterminationType: determinationType,
		Confidence:        art.Confidence,
		ValidFrom:         now,
	}
	created := 0
	switch err := m.graph.CreateEdge(ctx, tenantID, controlID, artID, demandsEdge); {
	case errors.Is(err, graphdb.ErrEdgeExists):
		// An earlier run, of this or another job with identical content, already wrote this edge.
	case err != nil:
		return fmt.Errorf("materializer: creating DEMANDS edge %s->%s: %w", controlID, artID, err)
	default:
		created++
	}

	// Create IS_TYPE edge: (Artifact) -> (ArtifactType).
	typeID := strings.ToLower(art.Type.String())
	isTypeEdge := graphdb.Edge{
		ID:                graphdb.DerivedID("is-type", artID, typeID),
		Label:             isTypeLabel,
		DeterminedBy:      jobID,
		DeterminationType: determinationType,
		Confidence:        1.0,
		ValidFrom:         now,
	}
	switch err := m.graph.CreateEdge(ctx, tenantID, artID, typeID, isTypeEdge); {
	case errors.Is(err, graphdb.ErrEdgeExists):
		// An earlier run, of this or another job with identical content, already wrote this edge.
	case err != nil:
		return fmt.Errorf("materializer: creating IS_TYPE edge %s->%s: %w", artID, typeID, err)
	default:
		created++
	}

	if m.edgeCounter != nil && created > 0 {
		m.edgeCounter.Add(ctx, int64(created), metric.WithAttributes(
			attribute.String("edge.label", "DEMANDS+IS_TYPE"),
		))
	}

	return nil
}

// checkStoredArtifact reports whether a node with want's ID is stored, and
// returns nil if none is or the stored node matches want, and otherwise the
// error compareArtifact reports.
func (m *GraphMaterializer) checkStoredArtifact(ctx context.Context, tenantID string, want graphdb.Node) (bool, error) {
	stored, err := m.graph.GetNode(ctx, tenantID, want.ID)
	switch {
	case errors.Is(err, graphdb.ErrNodeNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("materializer: checking existing Artifact %s for conflicts "+
			"(retry once the graph is reachable; re-running the job is safe): %w", want.ID, err)
	}
	if stored.Label != want.Label {
		return true, fmt.Errorf("materializer: cannot materialize Artifact %s for job %q, so nothing was written: %w",
			want.ID, want.CreatedBy,
			&graphdb.NodeIDConflictError{ID: want.ID, Label: want.Label, StoredLabel: stored.Label})
	}
	return true, compareArtifact(*stored, want)
}

// compareArtifact accepts stored if it holds the same content want would
// write: the same creation method and properties, ignoring ValidFrom and
// CreatedBy, so another job rediscovering an identical artifact is not a
// conflict. CreatedBy only picks the error message when the content differs.
func compareArtifact(stored, want graphdb.Node) error {
	differing := differingFields(stored, want)
	switch {
	case len(differing) == 0:
		return nil
	case stored.CreatedBy != want.CreatedBy:
		return fmt.Errorf("materializer: cannot materialize Artifact %s for job %q: job %q already materialized it "+
			"with different %s. Artifact IDs are shared across jobs and this materializer never replaces a stored "+
			"Artifact, so the stored node is kept; the graph has no per-node delete to remove it. To keep this "+
			"job's version instead, materialize job %q before job %q into a rebuilt tenant graph (drop and "+
			"recreate it, then re-import the catalogs so the Control nodes exist; see the upgrade note in "+
			"docs/dev/design-principles.md): %w",
			want.ID, want.CreatedBy, stored.CreatedBy, strings.Join(differing, ", "),
			want.CreatedBy, stored.CreatedBy, graphdb.ErrNodeExists)
	default:
		return fmt.Errorf("materializer: cannot materialize Artifact %s for job %q: the graph already holds it for "+
			"this job with different %s, so this job's stored results changed after they were materialized. "+
			"This is a bug to investigate, not something to retry: %w",
			want.ID, want.CreatedBy, strings.Join(differing, ", "), graphdb.ErrNodeExists)
	}
}

// differingFields lists the content fields in which stored differs from want.
// Every property is a string or float64, the types drivers read them back as,
// so values compare directly. On agedriver the comparison also assumes stored strings never contained the $cypher$ tag
// (the driver strips it on write) and that floats round-trip, which needs
// extra_float_digits >= 1 (the PostgreSQL 12+ default).
func differingFields(stored, want graphdb.Node) []string {
	var differing []string
	if stored.CreationMethod != want.CreationMethod {
		differing = append(differing, "creation_method")
	}
	for _, k := range slices.Sorted(maps.Keys(want.Properties)) {
		got, ok := stored.Properties[k]
		if !ok || got != want.Properties[k] {
			differing = append(differing, k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(stored.Properties)) {
		if _, ok := want.Properties[k]; !ok && strings.HasPrefix(k, "prop_") {
			differing = append(differing, k)
		}
	}
	return differing
}

func (m *GraphMaterializer) startSpan(ctx context.Context, name string) (context.Context, trace.Span) {
	if m.tracer == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	return m.tracer.Start(ctx, name)
}
