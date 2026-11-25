package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
	"github.com/complytime-labs/crosscodex/internal/analyzer/critique"
	"github.com/complytime-labs/crosscodex/internal/analyzer/relationship"
	"github.com/complytime-labs/crosscodex/internal/analyzer/requires"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/jobconfig"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
)

// Request bounds for RenderPrompt. They are security ceilings on what one
// request may make the service hold and render, not tuning: a real job's
// justifications number at most its proposing panel's size and are stored
// at most intanalyzer.MaxStoredJustificationBytes each, and IDs are short.
const (
	maxRenderIDBytes            = 256
	maxRenderJustifications     = 64
	maxRenderJustificationBytes = intanalyzer.MaxStoredJustificationBytes
)

// renderJobPlaceholder is the job_id parameter of a render without a job.
// The pair analyzers require one, and no prompt shows it.
const renderJobPlaceholder = "prompt-render"

// ErrRenderCallsNoLLM is returned by every call to the LLM client RenderPrompt
// builds analyzers with: rendering must never reach a model.
var ErrRenderCallsNoLLM = errors.New("RenderPrompt calls no LLM")

// noLLM is the LLM client RenderPrompt gives the analyzers it builds. They
// do not call it to render; if one ever did, the call fails.
type noLLM struct{}

func (noLLM) Complete(context.Context, *llmclient.CompletionRequest) (*llmclient.CompletionResponse, error) {
	return nil, ErrRenderCallsNoLLM
}

func (noLLM) Embed(context.Context, *llmclient.EmbeddingRequest) (*llmclient.EmbeddingResponse, error) {
	return nil, ErrRenderCallsNoLLM
}

func (noLLM) Health(context.Context) error { return ErrRenderCallsNoLLM }

func (noLLM) Close() error { return nil }

// PromptJobReader reads the stored job and analyzer results RenderPrompt
// renders from; Store implements it.
type PromptJobReader interface {
	GetJob(ctx context.Context, jobID string) (*Job, error)
	GetCompletedAnalysisResults(ctx context.Context, jobID string, analyzerNames []string) (map[string]*analyzer.Output, error)
}

// RenderPromptSources is what ServeRenderPrompt renders from. Prompts and
// Analysis must be the prompt registry and the analysis settings
// NewProductionRegistry builds the analyzers from, so the prompt rendered is
// the one a job sends. Any nil field refuses every request (fail closed).
// Logger receives the store errors withheld from the caller.
type RenderPromptSources struct {
	Prompts         prompt.Registry
	Analysis        *config.AnalysisConfig
	CatalogNames    CatalogNameReader
	CatalogControls CatalogControlsReader
	Jobs            PromptJobReader
	Logger          *slog.Logger
}

// RenderPrompt renders the prompt one analyzer sends for one control pair,
// from the sources WithPromptRegistry, WithAnalysisConfig,
// WithCatalogNameReader and WithCatalogControlsReader wire. See
// ServeRenderPrompt.
func (s *Service) RenderPrompt(ctx context.Context, req *connect.Request[pb.RenderPromptRequest]) (*connect.Response[pb.RenderPromptResponse], error) {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.RenderPrompt")
	defer span.End()
	span.SetAttributes(attribute.String("analyzer", req.Msg.GetAnalyzer()), attribute.String("job.id", req.Msg.GetJobId()))

	resp, err := ServeRenderPrompt(ctx, RenderPromptSources{
		Prompts: s.prompts, Analysis: s.analysisCfg, CatalogNames: s.catalogNames,
		CatalogControls: s.catalogControls, Jobs: s.store, Logger: s.logger,
	}, req)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		return nil, err
	}
	span.SetAttributes(attribute.String("prompt.name", resp.Msg.GetPromptName()),
		attribute.String("prompt.version", resp.Msg.GetPromptVersion()))
	span.SetStatus(otelcodes.Ok, "")
	return resp, nil
}

// ServeRenderPrompt answers a RenderPrompt request. It is the one
// implementation behind Service.RenderPrompt and the CLI's embedded mode.
//
// The analyzer is built as NewProductionRegistry builds it, from
// src.Prompts and src.Analysis, with an LLM client that fails if called,
// and the messages are its own Render's: the method its GenerateWork renders
// every task with. The pair's parameters are pairAnalyzerParameters' over
// the controls a job would read: the job's catalogs with job_id, else the
// source and target catalogs. With job_id each control's framework is its
// catalog's name as the job recorded it and the types and levels come from
// the job's stored classify result; without it they are the catalogs'
// current names and no type or level. Critique reviews the verdict the job
// accepted for the pair (CritiqueVerdictProvider), or the one the request
// gives; exactly one is required.
//
// The tenant is ctx's, as ServeCatalogControls checks it. A catalog, control
// or job that is not the tenant's, whether unknown or another tenant's, gets
// one fixed NotFound that names none of them, so the answer is no existence
// oracle. Store errors are logged with src.Logger and withheld (CWE-209).
func ServeRenderPrompt(ctx context.Context, src RenderPromptSources,
	req *connect.Request[pb.RenderPromptRequest]) (*connect.Response[pb.RenderPromptResponse], error) {
	return serveRenderPrompt(ctx, src, noLLM{}, req)
}

// renderNotFound is the one NotFound of RenderPrompt (see
// ServeRenderPrompt), built per call since a connect error carries
// mutable metadata.
func renderNotFound() error {
	return connect.NewError(connect.CodeNotFound, errors.New(
		"no prompt to render: a catalog, control or job named in the request is not one of your tenant's. "+
			`Find catalog IDs with "crosscodex catalog list", their control IDs with "crosscodex catalog controls <catalog-id>" `+
			`and check a job ID with "crosscodex run status <job-id>"`))
}

// serveRenderPrompt is ServeRenderPrompt with the LLM client the analyzers
// are built with, so specs can give one that fails them if called.
func serveRenderPrompt(ctx context.Context, src RenderPromptSources, llm llmclient.Client,
	req *connect.Request[pb.RenderPromptRequest]) (*connect.Response[pb.RenderPromptResponse], error) {
	msg := req.Msg
	tenantID, err := requestTenant(ctx, msg.GetTenantContext())
	if err != nil {
		return nil, err
	}
	if err := validateRenderRequest(msg); err != nil {
		return nil, err
	}
	if src.Prompts == nil || src.Analysis == nil || src.CatalogNames == nil || src.CatalogControls == nil || src.Jobs == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New(
			"cannot render the prompt: the pipeline is not wired with the prompt registry, the analysis settings, "+
				"the catalog readers and the job store it renders from. An operator must wire them "+
				"(pipeline.WithPromptRegistry, pipeline.WithAnalysisConfig, pipeline.WithCatalogNameReader, pipeline.WithCatalogControlsReader)"))
	}
	logger := src.Logger
	if logger == nil {
		logger = slog.Default()
	}
	unreadable := connect.NewError(connect.CodeUnavailable, errors.New(
		"cannot render the prompt: the catalog or job store could not be read. "+
			"Retry the request; if it keeps failing, ask an operator to check the database"))
	storeFailed := func(what string, err error) error {
		logger.ErrorContext(ctx, what, "operation", "RenderPrompt", "tenant", tenantID,
			"job_id", msg.GetJobId(), "error", err)
		return unreadable
	}

	// The job, when named, decides the catalogs read and their framework
	// names, as runAnalysis does for the job.
	jobID := msg.GetJobId()
	var cats jobconfig.Catalogs
	var catalogIDs []string
	if jobID != "" {
		job, err := src.Jobs.GetJob(ctx, jobID)
		if errors.Is(err, ErrNotFound) {
			return nil, renderNotFound()
		}
		if err != nil {
			return nil, storeFailed("job lookup failed", err)
		}
		if job.TenantID != tenantID {
			return nil, renderNotFound()
		}
		var ok bool
		if cats, ok = jobconfig.ExtractCatalogs(job.Config); !ok {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
				"cannot render a prompt of job %s: its stored config names no catalog, so it never ran analysis. "+
					"Render from catalogs instead: drop job_id and give source_catalog_id and target_catalog_id", jobID))
		}
		catalogIDs = cats.CatalogIDs()
	}
	sourceCatalog, targetCatalog := msg.GetSourceCatalogId(), msg.GetTargetCatalogId()
	if jobID != "" {
		if sourceCatalog == "" {
			sourceCatalog = cats.SourceID
		}
		if targetCatalog == "" {
			targetCatalog = cats.TargetID
		}
	}
	for _, c := range []struct{ id, field string }{{sourceCatalog, "source_catalog_id"}, {targetCatalog, "target_catalog_id"}} {
		switch {
		case c.id == "": // only without a job
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
				`%s is required without job_id: name the imported catalog the control is in (see "crosscodex catalog list"), `+
					"or give job_id to use the job's catalogs", c.field))
		case jobID != "" && !slices.Contains(catalogIDs, c.id):
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(
				"%s %q is not a catalog of job %s, whose catalogs are %s: the job's prompts were built from those. "+
					"Name one of them, or drop %s to use the job's", c.field, c.id, jobID, strings.Join(catalogIDs, " and "), c.field))
		}
	}
	if jobID == "" {
		catalogIDs = []string{sourceCatalog}
		if targetCatalog != sourceCatalog {
			catalogIDs = append(catalogIDs, targetCatalog)
		}
	}

	frameworks := make(map[string]string, len(catalogIDs))
	byCatalog := make(map[string][]*pb.Control, len(catalogIDs))
	var controls []*pb.Control
	for _, catalogID := range catalogIDs {
		snapshot, composed, err := tenantCatalogControls(ctx, src.CatalogNames, src.CatalogControls, tenantID, catalogID)
		if errors.Is(err, ErrCatalogNotFound) {
			return nil, renderNotFound()
		}
		if err != nil {
			return nil, storeFailed("catalog read failed", err)
		}
		frameworks[catalogID] = promptFramework(snapshot, cats, catalogID)
		byCatalog[catalogID] = composed
		controls = append(controls, composed...)
	}
	hasControl := func(catalogID, controlID string) bool {
		return slices.ContainsFunc(byCatalog[catalogID], func(c *pb.Control) bool { return c.GetControlId() == controlID })
	}
	sourceID, targetID := msg.GetSourceControlId(), msg.GetTargetControlId()
	if !hasControl(sourceCatalog, sourceID) || !hasControl(targetCatalog, targetID) {
		return nil, renderNotFound()
	}

	var classify *analyzer.Output
	paramsJobID := renderJobPlaceholder
	if jobID != "" {
		paramsJobID = jobID
		// As runAnalysis passes allOutputs["classify"].
		stored, err := src.Jobs.GetCompletedAnalysisResults(ctx, jobID, []string{"classify"})
		if err != nil {
			return nil, storeFailed("classify result read failed", err)
		}
		classify = stored["classify"]
	}
	params, err := pairAnalyzerParameters(paramsJobID, controls, frameworks, classify)
	if err != nil {
		return nil, storeFailed("stored classify result unreadable", err)
	}

	analysisCfg := *src.Analysis
	var promptName string
	var render func(*prompt.PromptSpec) ([]llmclient.ChatMessage, error)
	switch msg.GetAnalyzer() {
	case results.RelationshipAnalyzer:
		a := relationship.New(llm, src.Prompts, nil, analysisCfg.Relationship)
		promptName = a.PromptName()
		render = func(spec *prompt.PromptSpec) ([]llmclient.ChatMessage, error) {
			return a.Render(spec, relationship.CandidatePair{SourceControlID: sourceID, TargetControlID: targetID}, params)
		}
	case results.RequiresAnalyzer:
		a := requires.New(llm, src.Prompts, nil, analysisCfg.Requires)
		promptName = a.PromptName()
		render = func(spec *prompt.PromptSpec) ([]llmclient.ChatMessage, error) {
			return a.Render(spec, requires.RequiresPair{SourceControlID: sourceID, TargetControlID: targetID}, params)
		}
	default: // critique; validateRenderRequest admits no other analyzer
		v, err := critiqueVerdict(ctx, src.Jobs, tenantID, msg, logger)
		if err != nil {
			return nil, err
		}
		a := critique.New(llm, src.Prompts, nil, analysisCfg.Critique)
		promptName = a.PromptName()
		render = func(spec *prompt.PromptSpec) ([]llmclient.ChatMessage, error) { return a.Render(spec, v, params) }
	}

	spec, err := src.Prompts.Resolve(ctx, promptName)
	if err != nil {
		logger.ErrorContext(ctx, "prompt resolution failed", "operation", "RenderPrompt", "tenant", tenantID,
			"prompt", promptName, "error", err)
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf(
			"cannot render the prompt: the prompt registry could not resolve prompt %q. "+
				"Ask an operator to check the prompt configuration (crosscodex prompt show %s)", promptName, promptName))
	}
	messages, err := render(spec)
	if errors.Is(err, intanalyzer.ErrMissingControlText) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"cannot render the %s prompt for %s -> %s: a control has no statement, so no job sends a prompt for this pair: %w",
			msg.GetAnalyzer(), sourceID, targetID, err))
	}
	if err != nil {
		logger.ErrorContext(ctx, "prompt rendering failed", "operation", "RenderPrompt", "tenant", tenantID,
			"prompt", promptName, "error", err)
		return nil, connect.NewError(connect.CodeInternal, errors.New("cannot render the prompt: its template could not be filled in"))
	}

	out := make([]*pb.PromptMessage, len(messages))
	for i, m := range messages {
		out[i] = &pb.PromptMessage{Role: m.Role, Content: m.Content}
	}
	return connect.NewResponse(&pb.RenderPromptResponse{
		Messages: out, PromptName: spec.Name, PromptVersion: spec.Version,
	}), nil
}

// validateRenderRequest checks what a RenderPrompt request may ask for,
// before any store is read, so its answers depend on the request alone.
func validateRenderRequest(msg *pb.RenderPromptRequest) error {
	invalid := func(format string, args ...any) error {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
	}
	analyzers := []string{results.RelationshipAnalyzer, results.RequiresAnalyzer, results.CritiqueAnalyzer}
	if !slices.Contains(analyzers, msg.GetAnalyzer()) {
		return invalid("analyzer %q renders no pair prompt: name one of %s", intanalyzer.CleanDisplay(msg.GetAnalyzer(), maxRenderIDBytes),
			strings.Join(analyzers, ", "))
	}
	for _, f := range []struct{ field, value string }{
		{"source_control_id", msg.GetSourceControlId()}, {"target_control_id", msg.GetTargetControlId()},
	} {
		if f.value == "" {
			return invalid(`%s is required: name the control as "crosscodex catalog controls <catalog-id>" lists it`, f.field)
		}
	}
	for _, f := range []struct{ field, value string }{
		{"source_catalog_id", msg.GetSourceCatalogId()}, {"source_control_id", msg.GetSourceControlId()},
		{"target_catalog_id", msg.GetTargetCatalogId()}, {"target_control_id", msg.GetTargetControlId()},
		{"job_id", msg.GetJobId()},
	} {
		if len(f.value) > maxRenderIDBytes {
			return invalid("%s is %d bytes, over the limit of %d: no catalog, control or job ID is that long. Check the ID",
				f.field, len(f.value), maxRenderIDBytes)
		}
	}

	verdict := msg.GetVerdict()
	if msg.GetAnalyzer() != results.CritiqueAnalyzer {
		if verdict != nil {
			return invalid("a proposed verdict applies only to the critique analyzer, which reviews it; "+
				"drop --verdict and --justification to render the %s prompt", msg.GetAnalyzer())
		}
		return nil
	}
	if (msg.GetJobId() != "") == (verdict != nil) {
		return invalid("the critique prompt reviews one proposed verdict: give either --job <job-id> " +
			"(the verdict that job accepted for the pair) or --verdict <verdict> [--justification <text>]..., not both and not neither")
	}
	if verdict == nil {
		return nil
	}
	if verdictKind(verdict.GetVerdict()) == "" {
		return invalid("verdict %q is not a verdict critique reviews: give %s for a prerequisite, or one of the relationship types %s",
			intanalyzer.CleanDisplay(verdict.GetVerdict(), intanalyzer.MaxPromptValueBytes), requiresVerdict, strings.Join(reviewedRelationshipTypes(), ", "))
	}
	if n := len(verdict.GetJustifications()); n > maxRenderJustifications {
		return invalid("%d justifications given, over the limit of %d: a proposing panel gives one per vote. Give at most %d",
			n, maxRenderJustifications, maxRenderJustifications)
	}
	for i, j := range verdict.GetJustifications() {
		if len(j) > maxRenderJustificationBytes {
			return invalid("justification %d is %d bytes, over the %d bytes a job stores of one. Shorten it",
				i+1, len(j), maxRenderJustificationBytes)
		}
	}
	return nil
}

// reviewedRelationshipTypes are the relationship verdicts critique reviews:
// every type but NO_RELATIONSHIP, which proposes nothing.
func reviewedRelationshipTypes() []string {
	var names []string
	for _, t := range relationship.AllRelationshipTypes() {
		if t != relationship.RelNoRelationship {
			names = append(names, t.String())
		}
	}
	return names
}

// verdictKind is the critique kind of an explicit proposed verdict:
// KindRequires for requiresVerdict, KindRelationship for a reviewed
// relationship type, "" for anything else.
func verdictKind(verdict string) string {
	if verdict == requiresVerdict {
		return critique.KindRequires
	}
	if slices.Contains(reviewedRelationshipTypes(), verdict) {
		return critique.KindRelationship
	}
	return ""
}

// critiqueVerdict is the proposed verdict the critique prompt reviews: the
// request's, or the one job_id's panels accepted for the pair, read as the
// job's critique stage reads it.
func critiqueVerdict(ctx context.Context, jobs PromptJobReader, tenantID string, msg *pb.RenderPromptRequest,
	logger *slog.Logger) (critique.ProposedVerdict, error) {
	sourceID, targetID := msg.GetSourceControlId(), msg.GetTargetControlId()
	if v := msg.GetVerdict(); v != nil {
		return critique.ProposedVerdict{Kind: verdictKind(v.GetVerdict()), SourceControlID: sourceID, TargetControlID: targetID,
			Verdict: v.GetVerdict(), Justifications: v.GetJustifications()}, nil
	}
	jobID := msg.GetJobId()
	all, err := NewCritiqueVerdictProvider(jobs).Verdicts(ctx, tenantID, jobID)
	if err != nil {
		logger.ErrorContext(ctx, "critique verdicts read failed", "operation", "RenderPrompt", "tenant", tenantID,
			"job_id", jobID, "error", err)
		return critique.ProposedVerdict{}, connect.NewError(connect.CodeUnavailable, errors.New(
			"cannot render the prompt: the job's stored relationship and requires results could not be read. "+
				"Retry the request; if it keeps failing, ask an operator to check the database"))
	}
	var found []critique.ProposedVerdict
	for _, v := range all {
		if v.SourceControlID == sourceID && v.TargetControlID == targetID {
			found = append(found, v)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return critique.ProposedVerdict{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"job %s accepted no relationship or requires verdict for %s -> %s, so its critique stage reviewed nothing for the pair. "+
				"Check the pair's results, or render a verdict of your own with --verdict instead of --job", jobID, sourceID, targetID))
	case 2:
		if found[0].Kind == found[1].Kind {
			break
		}
		rel := found[slices.IndexFunc(found, func(v critique.ProposedVerdict) bool { return v.Kind == critique.KindRelationship })]
		return critique.ProposedVerdict{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"job %s accepted both a relationship (%s) and a requires verdict for %s -> %s, and its critique stage reviewed each "+
				"with its own prompt; a render shows one. Render each with --verdict %s or --verdict %s instead of --job, "+
				"giving the job's justifications with --justification", jobID, rel.Verdict, sourceID, targetID, rel.Verdict, requiresVerdict))
	}
	// As critique.GenerateWork refuses it (ErrInvalidVerdict): a verdict
	// kind listed twice for one pair.
	return critique.ProposedVerdict{}, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
		"job %s lists the same kind of verdict for %s -> %s more than once, so its stored results are corrupt and its "+
			"critique stage refused them. Re-run the job", jobID, sourceID, targetID))
}
