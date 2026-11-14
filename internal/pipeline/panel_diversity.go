package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// ErrPanelNotIndependent reports a panel (relationship, requires, critique)
// whose aliases answer as the same upstream model, or as one the gateway
// does not name. Their votes would measure one model's self-consistency, not
// agreement between models.
var ErrPanelNotIndependent = errors.New("panel models are not independent")

// Outcomes of a panel check: the values of the outcome attribute of the
// panel_checks counter. independent is the only one that is not a finding.
const (
	panelIndependent = "independent"
	panelShared      = "shared"
	panelAllowed     = "allowed"
	panelUnknown     = "unknown"
	panelFailed      = "failed"
)

// Values of the panel.independence byproduct of an analyzer's link.
const (
	byproductPanelIndependence = "panel.independence"
	byproductPanelModels       = "panel.models"
	byproductPanelAllowShared  = "panel.allow_shared"
	byproductPanelAliases      = "panel.aliases"
	panelRecordIndependent     = "independent"
	panelRecordAllowed         = "allowed shared or unknown upstream model"
	panelRecordUnchecked       = "panel independence unchecked"
	panelRecordNotRecorded     = "panel independence not recorded"
)

// panelSpec is one stage's panel: the aliases whose votes the stage counts
// and whether analysis.<stage>.allow_shared_panel_models is set.
type panelSpec struct {
	Stage       string
	Aliases     []string
	AllowShared bool
}

// panelSpecOf returns the panel the analyzer called name declares, and false
// when it declares none or name is no registered analyzer (a pipeline stage,
// or an analyzer disabled since the job was created).
func (s *Service) panelSpecOf(name string) (panelSpec, bool, error) {
	if s.registry == nil {
		return panelSpec{}, false, nil
	}
	ra, err := s.registry.Get(name)
	if errors.Is(err, analyzer.ErrNotFound) {
		return panelSpec{}, false, nil
	}
	if err != nil {
		return panelSpec{}, false, fmt.Errorf("panel check: analyzer %q: %w", name, err)
	}
	p, ok := analyzer.AsPanelled(ra)
	if !ok {
		return panelSpec{}, false, nil
	}
	panel := p.Panel()
	return panelSpec{Stage: name, Aliases: panel.Aliases, AllowShared: panel.AllowShared}, true, nil
}

// Where an alias's upstream model came from, recorded per alias.
const (
	sourceReply   = "reply"
	sourceCatalog = "catalog"
	sourceUnknown = "unknown"
)

// panelIdentity is what the check found out about one alias: its upstream
// model ("" when unknown), where that came from, the deployment ID the
// gateway served the probe with (never the deployment's URL or key), and,
// for an unknown model, why the catalog could not name it.
type panelIdentity struct {
	Model        string `json:"model"`
	Source       string `json:"source"`
	DeploymentID string `json:"deployment_id,omitempty"`
	Fault        string `json:"fault,omitempty"`
}

// modelsOf returns the alias-to-model map judgePanel takes.
func modelsOf(ids map[string]panelIdentity) map[string]string {
	if ids == nil {
		return nil
	}
	models := make(map[string]string, len(ids))
	for alias, id := range ids {
		models[alias] = id.Model
	}
	return models
}

// PanelProbeClient is what the panel diversity check needs of the gateway
// client: completions to probe each alias, and the gateway's model catalog,
// because a LiteLLM gateway answers a completion with the alias itself and
// names the model behind it only there.
type PanelProbeClient interface {
	llmclient.Client
	llmclient.ModelCatalog
}

// modelCatalogPath is where a LiteLLM gateway publishes its catalog, named in
// the actionable messages.
const modelCatalogPath = "/v1/model/info"

// ErrPanelCatalogDenied reports a gateway that refused the key's request for
// its model catalog, so the model behind each panel alias cannot be read.
var ErrPanelCatalogDenied = errors.New("the LLM gateway refused to share its model catalog")

// sameUpstream reports whether two upstream model identities name one model:
// equal ignoring case; or one is the other with leading "<segment>/" provider
// prefixes removed (hosted_vllm/Org/m against Org/m); or both are equal once
// their first segment is dropped (openai/m against hosted_vllm/m).
func sameUpstream(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if a == b || strings.HasSuffix(a, "/"+b) || strings.HasSuffix(b, "/"+a) {
		return true
	}
	_, restA, foundA := strings.Cut(a, "/")
	_, restB, foundB := strings.Cut(b, "/")
	return foundA && foundB && restA == restB
}

// sharedName is how a shared model is named in the error: the one name when
// both aliases report it identically, else both spellings.
func sharedName(a, b string) string {
	if a == b {
		return a
	}
	return a + " and " + b + " (the same model)"
}

// judgePanel classifies the models the aliases of spec reported (alias to
// upstream model, "" for an unknown one) and returns the outcome. A panel of
// fewer than two distinct aliases is one model by construction, so it is
// shared. A shared or unknown model is an error wrapping
// ErrPanelNotIndependent unless spec.AllowShared is set; the outcome is then
// panelAllowed.
func judgePanel(spec panelSpec, models map[string]string) (string, error) {
	setting := "analysis." + spec.Stage + ".allow_shared_panel_models"
	if distinct := slices.Compact(slices.Sorted(slices.Values(spec.Aliases))); len(distinct) < 2 {
		if spec.AllowShared {
			return panelAllowed, nil
		}
		return panelShared, &tenantError{sentinel: ErrPanelNotIndependent, message: fmt.Sprintf(
			"stage %q stopped: its panel has a single alias (%s), so its votes would only measure one model's "+
				"self-consistency, not agreement between models. List panel-1, panel-2 and panel-3 in analysis.%s.models, "+
				"each pointing at a different model in the LLM gateway config, or set %s: true to run a single-model panel.",
			spec.Stage, strings.Join(distinct, ", "), spec.Stage, setting)}
	}
	for i, a := range spec.Aliases {
		for _, b := range spec.Aliases[i+1:] {
			if models[a] == "" || models[b] == "" || !sameUpstream(models[a], models[b]) {
				continue
			}
			if spec.AllowShared {
				return panelAllowed, nil
			}
			return panelShared, &tenantError{sentinel: ErrPanelNotIndependent, message: fmt.Sprintf(
				"stage %q stopped: panel aliases %s and %s both answered as %s, so their votes are not independent and "+
					"confidence would only measure one model's self-consistency. Point the aliases at different models in the "+
					"LLM gateway config, or set %s: true to run a single-model panel.",
				spec.Stage, a, b, sharedName(models[a], models[b]), setting)}
		}
	}
	for _, a := range spec.Aliases {
		if models[a] != "" {
			continue
		}
		if spec.AllowShared {
			return panelAllowed, nil
		}
		return panelUnknown, &tenantError{sentinel: ErrPanelNotIndependent, message: fmt.Sprintf(
			"stage %q stopped: the upstream model behind panel alias %s is unknown: the gateway did not report the model behind "+
				"the alias (a LiteLLM gateway exposes it at %s), so panel independence cannot be verified. Make the gateway "+
				"report the model behind each alias, or set %s: true to run without the check.",
			spec.Stage, a, modelCatalogPath, setting)}
	}
	return panelIndependent, nil
}

// checkPanelDiversity probes every alias of panel once and refuses the panel
// (an error wrapping ErrPanelNotIndependent) when it has fewer than two
// distinct aliases, when two aliases answer as the same upstream model, or
// when an alias's upstream model is unknown, unless
// panel.AllowShared. It returns what it found for each alias (an unknown
// model is "") for the job's provenance. A model catalog read that fails is a
// failure of the stage, unless panel.AllowShared, which records the alias as
// unknown with the fault. A probe that fails
// returns the error wrapped, naming the alias. Aliases are checked within
// one stage only: stages may reuse the same aliases.
func (s *Service) checkPanelDiversity(ctx context.Context, tenantID, jobID string, panel panelSpec) (map[string]panelIdentity, error) {
	ctx, span := telemetry.StartSpan(s.tracer, ctx, "pipeline.CheckPanelDiversity")
	defer span.End()
	span.SetAttributes(attribute.String("tenant.id", tenantID), attribute.String("job.id", jobID),
		attribute.String("stage", panel.Stage), attribute.Bool("panel.allow_shared", panel.AllowShared))

	outcome := panelFailed
	defer func() {
		if s.panelChecks != nil {
			s.panelChecks.Add(ctx, 1, metric.WithAttributes(attribute.String("tenant.id", tenantID),
				attribute.String("stage", panel.Stage), attribute.String("outcome", outcome)))
		}
	}()

	ids := make(map[string]panelIdentity, len(panel.Aliases))
	var catalog *modelCatalogRead
	for _, alias := range panel.Aliases {
		if _, done := ids[alias]; done {
			continue
		}
		observed, deployment, err := s.probeObserved(ctx, s.panelProbe, tenantID, jobID,
			analyzer.ModelUse{Alias: alias, Kind: analyzer.ModelCompletion})
		if err != nil {
			err = fmt.Errorf("panel check: probing alias %q of stage %q: %w", alias, panel.Stage, err)
			span.RecordError(err)
			span.SetStatus(codes.Error, "panel probe failed")
			return nil, err
		}
		id := panelIdentity{Source: sourceUnknown, DeploymentID: deployment}
		switch {
		case observed != "" && observed != alias && usableModelName(observed):
			id.Model, id.Source = observed, sourceReply
		case deployment != "":
			if catalog == nil {
				catalog = &modelCatalogRead{}
				catalog.deployments, catalog.err = s.panelProbe.ModelCatalog(ctx)
			}
			switch err := catalog.err; {
			case err == nil:
				if model := catalog.upstreamOf(alias, deployment); model != "" {
					id.Model, id.Source = model, sourceCatalog
				}
			case errors.Is(err, llmclient.ErrModelCatalogUnsupported):
				id.Fault = "catalog not published"
			case panel.AllowShared:
				span.RecordError(err)
				id.Fault = catalogFault(err)
			default:
				span.RecordError(err)
				span.SetStatus(codes.Error, "model catalog read failed")
				if errors.Is(err, llmclient.ErrAuthentication) {
					return nil, &tenantError{sentinel: ErrPanelCatalogDenied, message: fmt.Sprintf(
						"stage %q stopped: the LLM gateway refused the API key's request for its model catalog (%s), so the model "+
							"behind each panel alias cannot be read. Let the key read %s, or set analysis.%s.allow_shared_panel_models: "+
							"true to run without the check.", panel.Stage, modelCatalogPath, modelCatalogPath, panel.Stage)}
				}
				return nil, fmt.Errorf("panel check: reading the model catalog for stage %q: %w", panel.Stage, err)
			}
		}
		ids[alias] = id
	}
	models := modelsOf(ids)
	var err error
	if outcome, err = judgePanel(panel, models); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetStatus(codes.Ok, outcome)
	return ids, nil
}

// catalogFault names why the model catalog could not be read, for the record.
func catalogFault(err error) string {
	if errors.Is(err, llmclient.ErrAuthentication) {
		return "catalog refused"
	}
	if errors.Is(err, llmclient.ErrGatewayUnavailable) {
		return "catalog unavailable"
	}
	return "catalog unreadable"
}

// modelCatalogRead is one read of the gateway's model catalog, shared by the
// aliases of a check.
type modelCatalogRead struct {
	deployments []llmclient.ModelDeployment
	err         error
}

// upstreamOf returns the upstream model of the deployment the gateway served
// alias with: the entry named alias whose ID is deployment. It is "" (unknown)
// when the catalog is unsupported, has no such entry, or names an unusable
// model. Only the model is kept; the entry's other fields are never read.
func (r *modelCatalogRead) upstreamOf(alias, deployment string) string {
	for _, d := range r.deployments {
		if d.Alias == alias && d.ID == deployment && usableModelName(d.Model) {
			return d.Model
		}
	}
	return ""
}

// panelRecord is what the panel diversity check found for a stage: whether
// the panel was independent, allowed through, or not checked, the model each
// alias reported, and whether the allowance was in effect. It is stored with
// the stage (Stage.PanelRecord) when the check runs, shown by run status and
// recorded in the stage's attestation link.
type panelRecord struct {
	Independence string            `json:"independence"`
	AllowShared  bool              `json:"allow_shared"`
	Models       map[string]string `json:"models,omitempty"`
	// Aliases is what the check found per alias: where its model came from
	// and the deployment ID the gateway served the probe with.
	Aliases map[string]panelIdentity `json:"aliases,omitempty"`
}

// panelRecordOf returns the record of panel whose probes reported models;
// models is nil for a panel that was not checked.
func panelRecordOf(panel panelSpec, ids map[string]panelIdentity) panelRecord {
	models := modelsOf(ids)
	if models == nil {
		return panelRecord{Independence: panelRecordUnchecked, AllowShared: panel.AllowShared}
	}
	rec := panelRecord{Independence: panelRecordIndependent, AllowShared: panel.AllowShared, Models: models, Aliases: ids}
	if outcome, _ := judgePanel(panel, models); outcome == panelAllowed {
		rec.Independence = panelRecordAllowed
	}
	return rec
}

// parsePanelRecord reads a stored Stage.PanelRecord. An empty one is a panel
// stage whose check was never recorded (the job predates it): it is reported
// as such, never left blank.
func parsePanelRecord(raw string) (panelRecord, error) {
	if raw == "" {
		return panelRecord{Independence: panelRecordNotRecorded}, nil
	}
	var rec panelRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return panelRecord{}, fmt.Errorf("reading a stored panel record: %w", err)
	}
	return rec, nil
}

func (r panelRecord) byProducts() map[string]any {
	by := map[string]any{byproductPanelIndependence: r.Independence, byproductPanelAllowShared: r.AllowShared}
	if r.Models != nil {
		by[byproductPanelModels] = r.Models
		by[byproductPanelAliases] = r.Aliases
	}
	return by
}

func (r panelRecord) toProto() *pb.PanelProvenance {
	out := &pb.PanelProvenance{Independence: r.Independence, AllowShared: r.AllowShared}
	for _, alias := range slices.Sorted(maps.Keys(r.Models)) {
		id := r.Aliases[alias]
		out.Models = append(out.Models, &pb.PanelModel{Alias: alias, ObservedModel: r.Models[alias],
			Source: id.Source, DeploymentId: id.DeploymentID, Fault: id.Fault})
	}
	return out
}

// storedPanelRecord returns the record of stage, a stage of the panel
// analyzer called name. ok is false when name is not a panel analyzer.
func (s *Service) storedPanelRecord(stage *Stage) (rec panelRecord, ok bool, err error) {
	if _, ok, err = s.panelSpecOf(stage.StageName); err != nil || !ok {
		return panelRecord{}, false, err
	}
	rec, err = parsePanelRecord(stage.PanelRecord)
	return rec, true, err
}

// checkPanels runs the diversity check for every panel analyzer among names,
// the analyzers this run will start, before any of them makes an analysis
// call, and keeps what it finds in exec for the analyzers' links. A refused
// panel fails its stage; a probe that fails is an ErrLLMUnavailable stage
// failure, as for the result cache's probes. Without a panel probe the
// panels are recorded as unchecked.
func (s *Service) checkPanels(ctx context.Context, job *Job, names []string, exec *jobExecution) error {
	tenantID, err := tenant.FromContext(ctx)
	if err != nil {
		return fmt.Errorf("panel check: %w", err)
	}
	for _, name := range names {
		panel, ok, err := s.panelSpecOf(name)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		var ids map[string]panelIdentity
		if s.panelProbe != nil {
			if ids, err = s.checkPanelDiversity(ctx, tenantID, job.JobID, panel); err != nil {
				switch {
				case ctx.Err() != nil:
					return fmt.Errorf("panel check: probing the panel of stage %q stopped: %w", name, ctx.Err())
				case errors.Is(err, ErrPanelNotIndependent) || errors.Is(err, ErrPanelCatalogDenied):
					s.failPipelineStage(ctx, job, name, err)
					return err
				default:
					return s.llmUnavailable(ctx, job, name, strings.Join(panel.Aliases, ","), tenantID, err)
				}
			}
		}
		rec := panelRecordOf(panel, ids)
		raw, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("panel check: encoding the panel record of stage %q: %w", name, err)
		}
		if err := s.store.SetStagePanelRecord(ctx, job.JobID, name, string(raw)); err != nil {
			return fmt.Errorf("panel check: storing the panel record of stage %q: %w", name, err)
		}
		if exec.panels == nil {
			exec.panels = map[string]panelRecord{}
		}
		exec.panels[name] = rec
	}
	return nil
}

// panelProvenance returns, by stage name, what run status shows for each
// panel stage in stages: its stored record, or "not recorded" for a
// completed stage that has none (a job that predates the record). A stage
// that has not started its analysis has no panel provenance yet. A record
// that cannot be read is logged and shown as not recorded.
func (s *Service) panelProvenance(stages []*Stage) map[string]*pb.PanelProvenance {
	out := map[string]*pb.PanelProvenance{}
	for _, st := range stages {
		rec, ok, err := s.storedPanelRecord(st)
		if err != nil {
			s.logger.Warn("a stored panel record is unreadable, so run status shows it as not recorded",
				"job_id", st.JobID, "stage", st.StageName, "error", err)
			rec, ok = panelRecord{Independence: panelRecordNotRecorded}, true
		}
		if !ok || (st.PanelRecord == "" && st.Status != StageStatusCompleted) {
			continue
		}
		out[st.StageName] = rec.toProto()
	}
	return out
}
