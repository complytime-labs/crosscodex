package artifacts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/consensus"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
	"github.com/complytime-labs/crosscodex/pkg/telemetry"
)

const (
	sameAsPromptName = "artifact_same_as"

	// DefaultAdjudicateMaxPairs is the default AdjudicateOptions.MaxPairs.
	DefaultAdjudicateMaxPairs = 200

	projectBatch = 100 // decided verdicts per Unprojected call

	maxPromptValueBytes = 200 // per untrusted value rendered into the prompt, before escaping

	maxErrorChars         = 1000
	maxJustificationChars = 2000
)

var (
	// promptBreaks matches runs of control characters (newlines and tabs
	// included) and Unicode separators (spaces, line and paragraph
	// separators).
	promptBreaks = regexp.MustCompile(`[\p{Cc}\p{Z}]+`)
	// promptInvisibles matches Unicode format characters (zero-width
	// characters, bidi overrides, tag characters). They render as nothing, so
	// removing them, rather than turning them into a space, leaves the text a
	// human reviewer sees; tag characters can otherwise smuggle hidden text
	// to the model.
	promptInvisibles = regexp.MustCompile(`\p{Cf}+`)
	// promptEscaper entity-encodes the characters that could open or close a
	// prompt tag.
	promptEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
)

// errLLMUnavailable marks a panel in which every call failed with one of
// systemLLMErrors. Run stops on it without recording a failure.
var errLLMUnavailable = errors.New("LLM unavailable")

// errNoChoices is the vote error for a completion with no choices. It is our
// own text, so panelError stores it as is.
var errNoChoices = errors.New("completion returned no choices")

// systemLLMErrors are failures of the LLM service or its configuration, not
// of one pair; they fail every pair alike.
var systemLLMErrors = []error{
	llmclient.ErrGatewayUnavailable, llmclient.ErrRateLimitExceeded, llmclient.ErrAuthentication,
	llmclient.ErrCredentialResolution, llmclient.ErrModelNotAllowed, llmclient.ErrNoGateway,
}

// llmErrors are all llmclient sentinels. A failure message names the
// sentinel instead of the full error, which can carry the gateway's reply;
// panelError replaces every other error with a fixed message.
var llmErrors = append(slices.Clone(systemLLMErrors),
	llmclient.ErrInvalidRequest, llmclient.ErrMaxTokensExceeded, llmclient.ErrResponseTooLarge)

// AdjudicateOptions configures one Adjudicator.Run.
type AdjudicateOptions struct {
	// MaxPairs is the most pairs one run sends to the panel. Must be positive.
	MaxPairs int
	// Now is the clock; it stamps leases, verdicts and retry times. Required.
	Now func() time.Time
	// DryRun reports the queue (Pending, Due, Unprojected) without leasing,
	// calling the LLM or writing.
	DryRun bool
	// Reproject marks every projected llm_panel or human confirmed or rejected
	// verdict of the tenant unprojected (VerdictStore.ResetProjection) and
	// writes them back to the graph, then stops without leasing or calling
	// the LLM. Use it after rebuilding the tenant's graph. Not with DryRun.
	Reproject bool
}

// AdjudicateResult reports one Adjudicator.Run. Under DryRun only Pending,
// Due and Unprojected are set.
//
// Leased counts every pair leased, including pairs a run that stops early
// leased but never finished. Failed and Abandoned count the failures the run
// recorded; a RecordFailure fenced off because another run re-leased the pair
// writes nothing but is still counted.
type AdjudicateResult struct {
	Reset int // Reproject: projected verdicts marked unprojected before recovery
	// Recovered counts the verdicts a projection pass (at the start or end of
	// Run, or Project) claimed and wrote to the graph, including Orphaned ones
	// and ones whose MarkProjected was fenced. Verdicts the pass skipped as
	// Busy or found replaced before claiming are not counted, nor are the
	// verdicts Run projects right after its own Decide.
	Recovered int
	Leased    int // pairs leased (Expired ones included)
	Expired   int // leased pairs skipped because their lease expired before their panel started; a later run retries them
	Confirmed int // verdicts recorded as confirmed
	Rejected  int // verdicts recorded as rejected
	Undecided int // split or below-threshold panels, recorded as undecided
	Failed    int // failed panels whose pair stays pending with a backoff
	Abandoned int // failed panels that reached max_attempts
	Dissent   int // panels on a pair a human decided meanwhile; opinion stored as dissent
	Stale     int // panels on a pair another run decided meanwhile
	Orphaned  int // confirmed verdicts whose groups are no longer in the graph
	// Fenced counts projections a newer verdict overtook, before the claim or
	// while it was held (the newer verdict stays queued and the pass retries),
	// and projections whose claim expired and was taken over by another pass.
	Fenced int
	// Busy counts projections skipped because another pass held a live claim
	// on the verdict or had already projected it; that pass finishes it.
	Busy int

	Pending     int // DryRun: pending pairs
	Due         int // DryRun: pending pairs leasable now
	Unprojected int // DryRun: decided verdicts waiting for projection
}

// Adjudicator runs the LLM panel over a tenant's pending SAME_AS candidates
// and projects its verdicts onto the graph. The verdict store is the source
// of truth; the graph only mirrors it, and every projection step is
// idempotent, so a run that stops part-way is completed by the next one.
type Adjudicator struct {
	g         graphdb.GraphDB
	store     VerdictStore
	llm       llmclient.Client
	prompts   prompt.Registry
	cfg       config.ArtifactAdjudicationConfig
	consensus *consensus.Computer

	tracer        trace.Tracer          // optional, nil-safe
	outcomes      metric.Int64Counter   // optional, nil-safe
	panelDuration metric.Int64Histogram // optional, nil-safe
	// inFlight is an up-down counter, not a gauge: Add is atomic, so
	// concurrent goroutines cannot record stale values out of order.
	inFlight metric.Int64UpDownCounter // optional, nil-safe
}

// AdjudicatorOption configures an Adjudicator.
type AdjudicatorOption func(*Adjudicator)

// WithAdjudicatorTelemetry enables tracing and metrics for the adjudicator.
// Either provider may be nil; nil providers are ignored.
func WithAdjudicatorTelemetry(tp trace.TracerProvider, mp metric.MeterProvider) AdjudicatorOption {
	return func(a *Adjudicator) {
		if tp != nil {
			a.tracer = tp.Tracer("crosscodex/internal/analyzer/artifacts")
		}
		if mp != nil {
			meter := mp.Meter("crosscodex")
			a.outcomes, _ = meter.Int64Counter("artifacts.adjudications.total",
				metric.WithDescription("Adjudicated SAME_AS pairs by outcome"))
			a.panelDuration, _ = meter.Int64Histogram("artifacts.adjudicate.panel.duration_ms",
				metric.WithDescription("LLM panel duration per pair in milliseconds"), metric.WithUnit("ms"))
			a.inFlight, _ = meter.Int64UpDownCounter("artifacts.adjudicate.completions.in_flight",
				metric.WithDescription("LLM completions the adjudicator has in flight"))
		}
	}
}

// NewAdjudicator returns an Adjudicator; cfg must have passed Validate. llm
// and prompts may be nil when every call is a DryRun or Reproject Run, or
// Project: none of them renders a prompt or calls the LLM.
func NewAdjudicator(g graphdb.GraphDB, store VerdictStore, llm llmclient.Client, prompts prompt.Registry, cfg config.ArtifactAdjudicationConfig, opts ...AdjudicatorOption) *Adjudicator {
	a := &Adjudicator{g: g, store: store, llm: llm, prompts: prompts, cfg: cfg,
		consensus: consensus.New(
			consensus.WithThreshold(cfg.ConsensusThreshold),
			consensus.WithMaxErrorRate(cfg.MaxErrorRate),
			// Without a minimum, Compute turns zero valid votes into a
			// 0.0-confidence result, so an outage would become a verdict.
			consensus.WithMinValidVotes(1),
		)}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Run first projects verdicts that earlier runs or humans recorded but did
// not project (with opts.Reproject it first marks every projected llm_panel or
// human confirmed or rejected verdict unprojected, so this step re-projects
// them, and stops there), then adjudicates up to opts.MaxPairs due pairs of
// tenantID, and finally projects again, so a human verdict that replaced a
// panel verdict while the run projected it reaches the graph before Run
// returns. Each verdict is recorded with a conditional Decide first and
// projected only if Decide applied it, so a human verdict that lands
// mid-panel wins.
//
// Every projection claims its pair first (VerdictStore.ClaimProjection, for
// lease_duration), writes to the graph, then releases the claim with the
// fenced MarkProjected; a pair claimed elsewhere is skipped (Busy). While a
// claim is live nobody else projects the pair, so a human verdict recorded
// during a panel projection is projected after the panel's graph writes, even
// if the panel run crashes before MarkProjected: the claim then expires and a
// later pass supersedes the panel's edge. A projection that fails keeps its
// claim, so its pair is projected again once lease_duration has passed, like
// a leased pair, unless it failed before writing anything: then the claim is
// released and the next pass retries at once. One window remains: a
// projection outliving lease_duration minus the clock skew between hosts
// (hosts must be NTP-synced, as for leases) can be overtaken by another pass,
// and a graph write it makes after that pass read the graph is not undone.
// Run --reproject repairs it.
//
// A panel without enough valid votes is retried after a backoff and abandoned
// at max_attempts. A pair whose lease expired before its panel started is
// skipped and counted as Expired. Store, graph and prompt errors stop the
// run, and so does a panel in which every call failed with a system-level
// LLM error (gateway unavailable, rate limit, authentication, credentials,
// model not allowed, no gateway): recording it would spend an attempt on
// every pair during an outage. Pairs a stopped run leased but did not finish
// become due again when their lease expires. Every LLM request of a run
// carries the run's ID as its JobID, so the run's LLM audit events share one
// audit subject; Run logs the ID with its summary.
func (a *Adjudicator) Run(ctx context.Context, tenantID string, opts AdjudicateOptions) (AdjudicateResult, error) {
	ctx, span := telemetry.StartSpan(a.tracer, ctx, "artifacts.adjudicate.run")
	defer span.End()
	// A run ID, not a pair key, because pair keys embed artifact names, which
	// may contain characters a NATS subject token cannot.
	runID := "artifact-adjudication-" + uuid.NewString()
	span.SetAttributes(attribute.String("tenant.id", tenantID), attribute.String("adjudicate.run_id", runID),
		attribute.Bool("adjudicate.dry_run", opts.DryRun))
	res, err := a.run(ctx, tenantID, runID, opts)
	span.SetAttributes(attribute.Int("adjudicate.leased", res.Leased), attribute.Int("adjudicate.recovered", res.Recovered),
		attribute.Int("adjudicate.fenced", res.Fenced), attribute.Int("adjudicate.busy", res.Busy))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	// The caller reports err; this records how far the run got.
	slog.InfoContext(ctx, "artifact adjudication run finished", "tenant.id", tenantID, "run.id", runID, "stopped_early", err != nil,
		"reset", res.Reset, "recovered", res.Recovered, "leased", res.Leased, "confirmed", res.Confirmed,
		"rejected", res.Rejected, "undecided", res.Undecided, "failed", res.Failed, "abandoned", res.Abandoned,
		"expired", res.Expired, "dissent", res.Dissent, "stale", res.Stale, "orphaned", res.Orphaned,
		"fenced", res.Fenced, "busy", res.Busy, "dry_run", opts.DryRun, "pending", res.Pending, "due", res.Due, "unprojected", res.Unprojected)
	return res, err
}

func (a *Adjudicator) run(ctx context.Context, tenantID, runID string, opts AdjudicateOptions) (AdjudicateResult, error) {
	var res AdjudicateResult
	if opts.MaxPairs <= 0 || opts.Now == nil || (opts.DryRun && opts.Reproject) {
		return res, fmt.Errorf("adjudicate artifacts: invalid options: MaxPairs (%d) must be positive, Now must be set, and DryRun excludes Reproject", opts.MaxPairs)
	}
	if opts.DryRun {
		c, err := a.store.Counts(ctx, tenantID, opts.Now())
		if err != nil {
			return res, fmt.Errorf("adjudicate artifacts: count queue: %w", err)
		}
		res.Pending, res.Due, res.Unprojected = c.Pending, c.Due, c.Unprojected
		return res, nil
	}
	if opts.Reproject {
		n, err := a.store.ResetProjection(ctx, tenantID)
		if err != nil {
			return res, fmt.Errorf("adjudicate artifacts: reset projection: %w", err)
		}
		res.Reset = n
	}
	if err := a.recoverProjections(ctx, tenantID, opts.Now, &res); err != nil {
		return res, err
	}
	if opts.Reproject {
		return res, nil
	}
	for res.Leased < opts.MaxPairs {
		// Pairs are leased lease_batch at a time, each for lease_duration, so a
		// lease rarely expires while its pair waits behind the rest of its
		// batch. One that does (slow panels) is skipped, since another run may
		// hold it now.
		pairs, err := a.store.Lease(ctx, tenantID, min(a.cfg.LeaseBatch, opts.MaxPairs-res.Leased), a.cfg.LeaseDuration, opts.Now())
		if err != nil {
			return res, fmt.Errorf("adjudicate artifacts: lease pairs: %w", err)
		}
		if len(pairs) == 0 {
			break
		}
		res.Leased += len(pairs)
		for _, p := range pairs {
			if err := a.adjudicatePair(ctx, tenantID, runID, p, opts.Now, &res); err != nil {
				return res, err
			}
		}
	}
	// A fence trip (a human verdict that replaced a panel verdict before or
	// while this run projected it) left the newer verdict queued; project it
	// before returning, so the graph does not keep the panel's edge until the
	// next run.
	if err := a.recoverProjections(ctx, tenantID, opts.Now, &res); err != nil {
		return res, err
	}
	return res, nil
}

// Project writes every decided verdict not yet in the graph and not claimed
// by another pass, panel and human alike, and stops: it neither leases pairs
// nor calls the LLM, so the Adjudicator may have a nil LLM client and prompt
// registry. now is the clock that times projection claims; it is required.
// It is the pass to run right after recording a human verdict. A pair whose
// previous verdict another pass is still projecting is left to that pass's
// end-of-run projection, or to a later pass once the claim expires. As for
// Run, a projection outliving lease_duration minus the clock skew between
// hosts (hosts must be NTP-synced, as for leases) may leave a stale edge that
// only Run with Reproject repairs.
func (a *Adjudicator) Project(ctx context.Context, tenantID string, now func() time.Time) (AdjudicateResult, error) {
	ctx, span := telemetry.StartSpan(a.tracer, ctx, "artifacts.adjudicate.project")
	defer span.End()
	span.SetAttributes(attribute.String("tenant.id", tenantID))
	var (
		res AdjudicateResult
		err error
	)
	if now == nil {
		err = errors.New("adjudicate artifacts: project: now must be set")
	} else {
		err = a.recoverProjections(ctx, tenantID, now, &res)
	}
	span.SetAttributes(attribute.Int("adjudicate.recovered", res.Recovered), attribute.Int("adjudicate.fenced", res.Fenced),
		attribute.Int("adjudicate.busy", res.Busy))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	// The caller reports err; this records how far the pass got.
	slog.InfoContext(ctx, "artifact verdict projection finished", "tenant.id", tenantID, "stopped_early", err != nil,
		"recovered", res.Recovered, "orphaned", res.Orphaned, "fenced", res.Fenced, "busy", res.Busy)
	return res, err
}

// recoverProjections projects every decided verdict that is not yet in the
// graph and not under a live claim, llm_panel and human alike, projectBatch at
// a time. A verdict a newer one replaced before it could be claimed, or while
// it was claimed (the MarkProjected fence), counts as Fenced and the newer
// verdict stays queued; so does one whose claim expired and was taken over.
// The loop returns only after a short batch without a fence trip, so when it
// returns no verdict it saw is left contradicting the latest one, except a
// verdict another live claim holds, which that claimant projects. It
// terminates because every fence trip, and so every retry, needs a decision
// or a claim made concurrently with this pass. Claims exclude other passes
// only while every projection finishes within lease_duration minus the clock
// skew between hosts (hosts must be NTP-synced, as for leases).
func (a *Adjudicator) recoverProjections(ctx context.Context, tenantID string, now func() time.Time, res *AdjudicateResult) error {
	for {
		batch, err := a.store.Unprojected(ctx, tenantID, projectBatch, now())
		if err != nil {
			return fmt.Errorf("adjudicate artifacts: load unprojected verdicts: %w", err)
		}
		fenced := res.Fenced
		for _, pr := range batch {
			wrote, err := a.project(ctx, tenantID, pr, now, res)
			if err != nil {
				return err
			}
			if wrote {
				res.Recovered++
			}
		}
		if len(batch) < projectBatch && res.Fenced == fenced {
			return nil
		}
	}
}

// record counts one pair's outcome and logs it.
func (a *Adjudicator) record(ctx context.Context, tenantID, key, outcome string) {
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("adjudicate.outcome", outcome))
	if a.outcomes != nil {
		a.outcomes.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
	}
	slog.InfoContext(ctx, "artifact pair adjudicated", "tenant.id", tenantID, "pair.key", key, "pair.ref", PairRef(key), "outcome", outcome)
}

func (a *Adjudicator) adjudicatePair(ctx context.Context, tenantID, runID string, p LeasedPair, now func() time.Time, res *AdjudicateResult) (err error) {
	ctx, span := telemetry.StartSpan(a.tracer, ctx, "artifacts.adjudicate.pair")
	span.SetAttributes(attribute.String("tenant.id", tenantID), attribute.String("pair.key", p.Key), attribute.String("pair.ref", PairRef(p.Key)))
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()
	if !now().Before(p.LeasedUntil) {
		res.Expired++
		a.record(ctx, tenantID, p.Key, "expired")
		return nil
	}
	start := time.Now()
	v, err := a.panel(ctx, tenantID, runID, p)
	if a.panelDuration != nil {
		a.panelDuration.Record(ctx, time.Since(start).Milliseconds())
	}
	// PostgreSQL keeps microseconds; truncating keeps a re-projected
	// timestamp identical to the first one.
	at := now().UTC().Truncate(time.Microsecond)
	var insufficient *consensus.ErrInsufficientVotes
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("adjudicate artifacts: %w", ctx.Err())
	case errors.Is(err, errLLMUnavailable):
		return fmt.Errorf("adjudicate artifacts: %w", err)
	case errors.As(err, &insufficient):
		return a.recordFailure(ctx, tenantID, p, err, at, res)
	case err != nil:
		return fmt.Errorf("adjudicate artifacts: pair %s: %w", PairRef(p.Key), err)
	}
	v.DecidedAt = at
	return a.commit(ctx, tenantID, p, v, now, res)
}

// panel asks every model SamplesPerModel times whether p's groups denote one
// artifact and turns the votes into a verdict. A failed call or an
// unparseable reply is an errored vote; too many of them return an error
// wrapping *consensus.ErrInsufficientVotes, unless every call failed with a
// systemLLMErrors error, which returns errLLMUnavailable instead.
func (a *Adjudicator) panel(ctx context.Context, tenantID, runID string, p LeasedPair) (Verdict, error) {
	c := p.Context
	rp, err := a.prompts.Render(ctx, sameAsPromptName, map[string]string{
		"artifact_type": promptValue(c.Type),
		"name_a":        promptValue(c.LowName),
		"name_b":        promptValue(c.HighName),
		"samples_a":     formatSamples(c.LowSamples),
		"samples_b":     formatSamples(c.HighSamples),
	})
	if err != nil {
		return Verdict{}, fmt.Errorf("render prompt %s: %w", sameAsPromptName, err)
	}
	messages := make([]llmclient.ChatMessage, len(rp.Messages))
	for i, m := range rp.Messages {
		messages[i] = llmclient.ChatMessage{Role: m.Role, Content: m.Content}
	}
	temperature := a.cfg.SamplingTemperature
	if a.cfg.SamplesPerModel <= 1 {
		temperature = 0
	}

	n := len(a.cfg.Models) * a.cfg.SamplesPerModel
	votes := make([]consensus.Vote, n)
	errs := make([]error, n)
	var eg errgroup.Group
	eg.SetLimit(a.cfg.PanelConcurrency)
	for i := range n {
		model, sample := a.cfg.Models[i/a.cfg.SamplesPerModel], i%a.cfg.SamplesPerModel
		eg.Go(func() error {
			voterID := fmt.Sprintf("%s__s%d", model, sample)
			if a.inFlight != nil {
				a.inFlight.Add(ctx, 1)
			}
			resp, err := a.llm.Complete(ctx, &llmclient.CompletionRequest{
				Model: model, Messages: messages, MaxTokens: a.cfg.MaxTokens, Temperature: &temperature,
				TenantID: tenantID, JobID: runID, PromptName: rp.Name, PromptVersion: rp.Version,
			})
			if a.inFlight != nil {
				a.inFlight.Add(ctx, -1)
			}
			switch {
			case err != nil:
				votes[i], errs[i] = consensus.Vote{VoterID: voterID}, err
			case len(resp.Choices) == 0:
				votes[i], errs[i] = consensus.Vote{VoterID: voterID}, errNoChoices
			default:
				votes[i] = ParseSameAsResponse(voterID, resp.Choices[0].Message.Content)
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return Verdict{}, err
	}

	result, err := a.consensus.Compute(votes)
	if err != nil {
		return Verdict{}, panelError(ctx, tenantID, p.Key, err, votes, errs)
	}

	ev := PanelEvidence{PromptName: rp.Name, PromptVersion: rp.Version, Models: slices.Clone(a.cfg.Models),
		Votes: make([]VoteRecord, n), ValidVotes: result.ValidVoteCount, TotalVotes: result.TotalVoteCount}
	for i, v := range votes {
		ev.Votes[i] = VoteRecord{VoterID: v.VoterID, Decision: v.Decision,
			Confidence: cleanText(v.Confidence, maxJustificationChars), Justification: cleanText(v.Justification, maxJustificationChars)}
	}
	status := VerdictUndecided
	// Compute breaks a tie as NO with fraction 0.5, so a split panel is
	// undecided whatever the threshold.
	if result.ConfidenceFraction > 0.5 && a.consensus.MeetsThreshold(result) {
		status = VerdictRejected
		if result.Decision {
			status = VerdictConfirmed
		}
	}
	return Verdict{Status: status, Confidence: result.ConfidenceFraction, Evidence: ev}, nil
}

// panelError explains why Compute rejected the votes. When every call failed
// with a system-level LLM error it returns errLLMUnavailable, which stops the
// run. Otherwise it adds, when no call failed, how many replies had no valid
// SAME: line, or else the first completion error redacted for storage in
// last_error (CWE-209: an llmclient error can carry the gateway's raw reply).
// It is stored as its llmclient sentinel's text, as "LLM API error (HTTP n)"
// for an *llmclient.APIError, as errNoChoices's own text, or else as
// "completion failed"; the full error is logged here, once.
func panelError(ctx context.Context, tenantID, key string, err error, votes []consensus.Vote, errs []error) error {
	var first error
	allSystem := true
	for _, e := range errs {
		if e == nil {
			allSystem = false
			continue
		}
		if first == nil {
			first = e
		}
		if !slices.ContainsFunc(systemLLMErrors, func(s error) bool { return errors.Is(e, s) }) {
			allSystem = false
		}
	}
	switch {
	case first == nil:
		unparsed := 0
		for _, v := range votes {
			if v.Decision == nil {
				unparsed++
			}
		}
		return fmt.Errorf("%w (%d of %d replies had no valid SAME: line)", err, unparsed, len(votes))
	case allSystem:
		return fmt.Errorf("%w: %w", errLLMUnavailable, first)
	}
	slog.WarnContext(ctx, "artifact adjudication: completion failed", "tenant.id", tenantID, "pair.key", key, "error", first)
	msg := "completion failed"
	var apiErr *llmclient.APIError
	if i := slices.IndexFunc(llmErrors, func(s error) bool { return errors.Is(first, s) }); i >= 0 {
		msg = llmErrors[i].Error()
	} else if errors.Is(first, errNoChoices) {
		msg = errNoChoices.Error()
	} else if errors.As(first, &apiErr) {
		msg = fmt.Sprintf("LLM API error (HTTP %d)", apiErr.StatusCode)
	}
	return fmt.Errorf("%w (first completion error: %s)", err, msg)
}

// CleanDisplay makes untrusted text safe to show as one line, in a prompt
// or a terminal: it repairs invalid UTF-8, removes Unicode format
// characters, collapses control and separator runs to one space, caps the
// result at maxBytes on a rune boundary and trims it. maxBytes must be >= 0;
// callers pass constants, so it is not clamped.
func CleanDisplay(s string, maxBytes int) string {
	// Repair invalid UTF-8 first: stripping an invisible rune from between the
	// bytes of a split sequence would otherwise splice them into a new rune such as U+202E.
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = promptBreaks.ReplaceAllString(promptInvisibles.ReplaceAllString(s, ""), " ")
	return strings.TrimSpace(cleanText(s, maxBytes))
}

// promptValue makes an untrusted value safe to render inside a prompt tag:
// CleanDisplay at maxPromptValueBytes, then entity-escaped so no character
// can close the tag. Escaping comes last, so the cap never splits an entity.
func promptValue(s string) string {
	return promptEscaper.Replace(CleanDisplay(s, maxPromptValueBytes))
}

// formatSamples renders member samples as prompt lines, each value through
// promptValue.
func formatSamples(samples []MemberSample) string {
	if len(samples) == 0 {
		return "- none recorded"
	}
	lines := make([]string, len(samples))
	for i, s := range samples {
		var details []string
		if owner := promptValue(s.OwnerRole); owner != "" {
			details = append(details, "owner: "+owner)
		}
		if freq := promptValue(s.Frequency); freq != "" {
			details = append(details, "frequency: "+freq)
		}
		lines[i] = "- " + promptValue(s.Name)
		if len(details) > 0 {
			lines[i] += " (" + strings.Join(details, "; ") + ")"
		}
	}
	return strings.Join(lines, "\n")
}

// commit records v and, when the store applied it, projects it onto the
// candidate edge Decide returns, which a reconcile run may have changed since
// p was leased.
func (a *Adjudicator) commit(ctx context.Context, tenantID string, p LeasedPair, v Verdict, now func() time.Time, res *AdjudicateResult) error {
	d, err := a.store.Decide(ctx, tenantID, p.Key, v)
	if err != nil {
		return fmt.Errorf("adjudicate artifacts: record verdict for pair %s: %w", PairRef(p.Key), err)
	}
	switch d.Outcome {
	case DecideHumanOwned:
		if err := a.store.RecordDissent(ctx, tenantID, p.Key, v); err != nil {
			return fmt.Errorf("adjudicate artifacts: record dissent for pair %s: %w", PairRef(p.Key), err)
		}
		res.Dissent++
		a.record(ctx, tenantID, p.Key, "dissent")
		return nil
	case DecideStale:
		res.Stale++
		a.record(ctx, tenantID, p.Key, "stale")
		return nil
	}
	switch v.Status {
	case VerdictConfirmed:
		res.Confirmed++
		a.record(ctx, tenantID, p.Key, "confirmed")
	case VerdictRejected:
		res.Rejected++
		a.record(ctx, tenantID, p.Key, "rejected")
	default:
		res.Undecided++
		a.record(ctx, tenantID, p.Key, "undecided")
		return nil
	}
	_, err = a.project(ctx, tenantID, Projection{
		Key: p.Key, LowGroupID: p.LowGroupID, HighGroupID: p.HighGroupID, CandidateEdgeID: d.CandidateEdgeID,
		Status: v.Status, Confidence: v.Confidence, Evidence: v.Evidence, DecidedAt: v.DecidedAt,
		DeterminationType: DeterminationLLMPanel,
	}, now, res)
	return err
}

// project claims pr's pair for lease_duration, writes pr to the graph and
// releases the claim through the MarkProjected fence; it reports whether it
// held the claim and wrote. A pair that no longer holds pr counts as Fenced,
// one claimed elsewhere or already projected as Busy; neither is written. A
// panel verdict and a human verdict project differently; see projectPanel
// and projectHuman. On an error before any graph write the claim is
// released; after a write that may have landed it stays until it expires.
func (a *Adjudicator) project(ctx context.Context, tenantID string, pr Projection, now func() time.Time, res *AdjudicateResult) (bool, error) {
	t := now()
	// PostgreSQL keeps microseconds; MarkProjected must name the claim exactly
	// as stored.
	until := t.Add(a.cfg.LeaseDuration).UTC().Truncate(time.Microsecond)
	claim, err := a.store.ClaimProjection(ctx, tenantID, pr, until, t)
	if err != nil {
		return false, fmt.Errorf("adjudicate artifacts: claim projection of pair %s: %w", PairRef(pr.Key), err)
	}
	switch claim {
	case ClaimReplaced:
		res.Fenced++
		a.record(ctx, tenantID, pr.Key, "fenced")
		return false, nil
	case ClaimBusy:
		res.Busy++
		a.record(ctx, tenantID, pr.Key, "busy")
		return false, nil
	}
	var wrote bool
	if pr.DeterminationType == DeterminationHuman {
		wrote, err = a.projectHuman(ctx, tenantID, pr, res)
	} else {
		wrote, err = a.projectPanel(ctx, tenantID, pr, res)
	}
	if err != nil {
		// Nothing reached the graph, so nothing needs a fence: release the
		// claim and let the next pass retry at once. After a write that may
		// have landed, the claim is kept until it expires, so a newer verdict
		// is projected after that write.
		if !wrote {
			if rerr := a.store.ReleaseClaim(ctx, tenantID, pr.Key, until); rerr != nil {
				err = errors.Join(err, fmt.Errorf("adjudicate artifacts: release projection claim on pair %s: %w", PairRef(pr.Key), rerr))
			}
		}
		return true, err
	}
	return true, a.markProjected(ctx, tenantID, pr, until, res)
}

// projectPanel makes the graph match an llm_panel verdict. A confirmation
// adds an llm_panel SAME_AS edge (ErrEdgeExists means an earlier projection
// wrote it) and then supersedes the candidate unless it is already
// superseded, so a crash between the two leaves both live until a later pass
// finishes the job once the claim expires. A rejection only supersedes the
// candidate, with the same exception. A confirmation whose groups are no
// longer in the graph has nothing to attach to: it counts as Orphaned and is
// marked projected, so it cannot block the queue. wrote reports whether a
// graph write that may have landed was made (a failed write may have landed
// too).
func (a *Adjudicator) projectPanel(ctx context.Context, tenantID string, pr Projection, res *AdjudicateResult) (wrote bool, err error) {
	if pr.Status == VerdictConfirmed {
		err := a.g.CreateEdge(ctx, tenantID, pr.LowGroupID, pr.HighGroupID, graphdb.Edge{
			ID: graphdb.DerivedID("same-as", DeterminationLLMPanel, pr.LowGroupID, pr.HighGroupID), Label: sameAsLabel,
			ValidFrom: pr.DecidedAt, DeterminedBy: adjudicatorName, DeterminationType: DeterminationLLMPanel,
			Confidence: pr.Confidence, Supersedes: pr.CandidateEdgeID,
			Properties: map[string]any{
				"prompt_name":    pr.Evidence.PromptName,
				"prompt_version": pr.Evidence.PromptVersion,
				"adjudicated_at": graphdb.FormatTime(pr.DecidedAt),
			},
		})
		switch {
		case errors.Is(err, graphdb.ErrNodeNotFound):
			res.Orphaned++
			return false, nil
		case err == nil:
			wrote = true
		case !errors.Is(err, graphdb.ErrEdgeExists):
			return true, fmt.Errorf("adjudicate artifacts: write SAME_AS for pair %s: %w", PairRef(pr.Key), err)
		}
	}
	// A candidate already superseded (by a human projection that ran
	// meanwhile, or by an earlier projection of this verdict) keeps its
	// valid_to: SupersedeFact overwrites it, which would rewrite history. A
	// missing candidate has nothing to supersede.
	c, err := a.g.GetEdge(ctx, tenantID, pr.CandidateEdgeID)
	switch {
	case errors.Is(err, graphdb.ErrEdgeNotFound):
		return wrote, nil
	case err != nil:
		return wrote, fmt.Errorf("adjudicate artifacts: read candidate edge of pair %s: %w", PairRef(pr.Key), err)
	case c.ValidTo != nil:
		return wrote, nil
	}
	if _, err := a.g.SupersedeFact(ctx, tenantID, graphdb.SupersedeRequest{EdgeID: pr.CandidateEdgeID, SupersededAt: pr.DecidedAt}); err != nil {
		return true, fmt.Errorf("adjudicate artifacts: supersede candidate edge of pair %s: %w", PairRef(pr.Key), err)
	}
	return true, nil
}

// projectHuman makes the graph match a human verdict. A confirmation adds a
// human SAME_AS edge whose ID includes the decision time, so a reversal and
// re-confirmation writes a new edge instead of colliding with its own
// superseded one; a confirmation whose groups are gone counts as Orphaned, as
// for the panel. Both verdicts then supersede every other live SAME_AS edge
// from low to high: the candidate, older-version candidates, an llm_panel
// edge (also one a racing run wrote after an earlier pass of this verdict,
// which the fence re-queued it for) and an earlier human edge. Each is
// superseded at the decision time, or at its own valid_from if that is later,
// so clock skew never ends an edge before it began. Edges already superseded
// are skipped, because SupersedeFact overwrites valid_to and would rewrite
// history; this also makes two passes projecting the same verdict write the
// same valid_to. wrote is as for projectPanel.
func (a *Adjudicator) projectHuman(ctx context.Context, tenantID string, pr Projection, res *AdjudicateResult) (wrote bool, err error) {
	keep := ""
	if pr.Status == VerdictConfirmed {
		keep = graphdb.DerivedID("same-as", DeterminationHuman, pr.LowGroupID, pr.HighGroupID, graphdb.FormatTime(pr.DecidedAt))
		err := a.g.CreateEdge(ctx, tenantID, pr.LowGroupID, pr.HighGroupID, graphdb.Edge{
			ID: keep, Label: sameAsLabel, ValidFrom: pr.DecidedAt, DeterminedBy: pr.DeterminedBy,
			DeterminationType: DeterminationHuman, Confidence: 1, Supersedes: pr.CandidateEdgeID,
		})
		switch {
		case errors.Is(err, graphdb.ErrNodeNotFound):
			res.Orphaned++
			return false, nil
		case err == nil:
			wrote = true
		case !errors.Is(err, graphdb.ErrEdgeExists):
			return true, fmt.Errorf("adjudicate artifacts: write human SAME_AS for pair %s: %w", PairRef(pr.Key), err)
		}
	}
	// Without AsOf, Traverse returns superseded edges too; ValidTo tells them
	// apart. A depth-1 outbound path is [low, target] with one edge.
	paths, err := a.g.Traverse(ctx, tenantID, graphdb.TraversalQuery{StartNode: pr.LowGroupID, Direction: "outbound",
		EdgeLabels: []string{sameAsLabel}, MaxDepth: 1})
	if err != nil {
		return wrote, fmt.Errorf("adjudicate artifacts: list SAME_AS edges for pair %s: %w", PairRef(pr.Key), err)
	}
	for _, p := range paths {
		if len(p.Edges) != 1 || len(p.Nodes) != 2 || p.Nodes[1].ID != pr.HighGroupID {
			continue
		}
		e := p.Edges[0]
		if e.ID == keep || e.ValidTo != nil {
			continue
		}
		// A reviewer's clock may run behind the adjudicator's; an edge never
		// ends before it began.
		at := pr.DecidedAt
		if e.ValidFrom.After(at) {
			at = e.ValidFrom
		}
		if _, err := a.g.SupersedeFact(ctx, tenantID, graphdb.SupersedeRequest{EdgeID: e.ID, SupersededAt: at}); err != nil {
			return true, fmt.Errorf("adjudicate artifacts: supersede a SAME_AS edge of pair %s: %w", PairRef(pr.Key), err)
		}
		wrote = true
	}
	return wrote, nil
}

// markProjected releases the claim until on pr's pair and marks pr
// projected, unless a newer verdict replaced it while it was projected or the
// claim expired and another pass took the pair over. Either way it counts a
// fence trip: a replaced verdict stays queued for a retry, and a pass that
// took the pair over projects it.
func (a *Adjudicator) markProjected(ctx context.Context, tenantID string, pr Projection, until time.Time, res *AdjudicateResult) error {
	ok, err := a.store.MarkProjected(ctx, tenantID, pr, until)
	if err != nil {
		return fmt.Errorf("adjudicate artifacts: mark pair %s projected: %w", PairRef(pr.Key), err)
	}
	if !ok {
		res.Fenced++
		a.record(ctx, tenantID, pr.Key, "fenced")
	}
	return nil
}

// recordFailure counts a failed panel and schedules the retry, or abandons
// the pair at max_attempts.
func (a *Adjudicator) recordFailure(ctx context.Context, tenantID string, p LeasedPair, cause error, at time.Time, res *AdjudicateResult) error {
	attempts := p.Attempts + 1
	f := Failure{Err: cleanText(cause.Error(), maxErrorChars), At: at, LeasedUntil: p.LeasedUntil,
		NextAttemptAt: at.Add(retryDelay(attempts, a.cfg.RetryBaseDelay, a.cfg.RetryMaxDelay)), Abandon: attempts >= a.cfg.MaxAttempts}
	if err := a.store.RecordFailure(ctx, tenantID, p.Key, f); err != nil {
		return fmt.Errorf("adjudicate artifacts: record failure for pair %s: %w", PairRef(p.Key), err)
	}
	if f.Abandon {
		res.Abandoned++
		a.record(ctx, tenantID, p.Key, "abandoned")
	} else {
		res.Failed++
		a.record(ctx, tenantID, p.Key, "failed")
	}
	return nil
}

// retryDelay is the wait after the attempts-th failure: base, doubling per
// failure, capped at maxDelay. It stops doubling before d could pass
// maxDelay, so d never overflows, whatever attempts is.
func retryDelay(attempts int, base, maxDelay time.Duration) time.Duration {
	d := base
	for i := 1; i < attempts; i++ {
		if d > maxDelay/2 {
			return maxDelay
		}
		d *= 2
	}
	return min(d, maxDelay)
}

// cleanText makes s storable in PostgreSQL text and jsonb, which reject NUL
// and invalid UTF-8, and cuts it to at most n bytes on a rune boundary.
func cleanText(s string, n int) string {
	s = strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "�")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
