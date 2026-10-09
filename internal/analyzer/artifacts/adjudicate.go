package artifacts

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/consensus"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/prompt"
)

const (
	sameAsPromptName = "artifact_same_as"

	// DefaultAdjudicateMaxPairs is the default AdjudicateOptions.MaxPairs.
	DefaultAdjudicateMaxPairs = 200

	// Pairs are leased leaseBatch at a time, each for leaseFor, so a lease
	// rarely expires while its pair waits behind the rest of its batch. One
	// that does (slow panels) is skipped, since another run may hold it now.
	leaseBatch = 10
	leaseFor   = 15 * time.Minute
	// panelConcurrency bounds one pair's completions in flight.
	panelConcurrency = 4

	retryBaseDelay = time.Minute
	retryMaxDelay  = 24 * time.Hour

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

// systemLLMErrors are failures of the LLM service or its configuration, not
// of one pair; they fail every pair alike.
var systemLLMErrors = []error{
	llmclient.ErrGatewayUnavailable, llmclient.ErrRateLimitExceeded, llmclient.ErrAuthentication,
	llmclient.ErrCredentialResolution, llmclient.ErrModelNotAllowed, llmclient.ErrNoGateway,
}

// llmErrors are all llmclient sentinels. A failure message names the
// sentinel instead of the full error, which can carry the gateway's reply.
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
}

// AdjudicateResult reports one Adjudicator.Run. Under DryRun only Pending,
// Due and Unprojected are set.
//
// Leased counts every pair leased, including pairs a run that stops early
// leased but never finished. Failed and Abandoned count the failures the run
// recorded; a RecordFailure fenced off because another run re-leased the pair
// writes nothing but is still counted.
type AdjudicateResult struct {
	Recovered int // decided verdicts from earlier runs projected by this run (Orphaned ones included)
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
}

// NewAdjudicator returns an Adjudicator; cfg must have passed Validate. llm
// may be nil when every Run is a DryRun, which never calls the LLM.
func NewAdjudicator(g graphdb.GraphDB, store VerdictStore, llm llmclient.Client, prompts prompt.Registry, cfg config.ArtifactAdjudicationConfig) *Adjudicator {
	return &Adjudicator{g: g, store: store, llm: llm, prompts: prompts, cfg: cfg,
		consensus: consensus.New(
			consensus.WithThreshold(cfg.ConsensusThreshold),
			consensus.WithMaxErrorRate(cfg.MaxErrorRate),
			// Without a minimum, Compute turns zero valid votes into a
			// 0.0-confidence result, so an outage would become a verdict.
			consensus.WithMinValidVotes(1),
		)}
}

// Run first projects verdicts that earlier runs recorded but did not project,
// then adjudicates up to opts.MaxPairs due pairs of tenantID. Each verdict is
// recorded with a conditional Decide first and projected only if Decide
// applied it, so a human verdict that lands mid-panel wins. A panel without
// enough valid votes is retried after a backoff and abandoned at
// max_attempts. A pair whose lease expired before its panel started is
// skipped and counted as Expired. Store, graph and prompt errors stop the
// run, and so does a panel in which every call failed with a system-level
// LLM error (gateway unavailable, rate limit, authentication, credentials,
// model not allowed, no gateway): recording it would spend an attempt on
// every pair during an outage. Pairs a stopped run leased but did not finish
// become due again when their lease expires.
func (a *Adjudicator) Run(ctx context.Context, tenantID string, opts AdjudicateOptions) (AdjudicateResult, error) {
	var res AdjudicateResult
	if opts.MaxPairs <= 0 || opts.Now == nil {
		return res, fmt.Errorf("adjudicate artifacts: invalid options: MaxPairs (%d) must be positive and Now must be set", opts.MaxPairs)
	}
	if opts.DryRun {
		c, err := a.store.Counts(ctx, tenantID, opts.Now())
		if err != nil {
			return res, fmt.Errorf("adjudicate artifacts: count queue: %w", err)
		}
		res.Pending, res.Due, res.Unprojected = c.Pending, c.Due, c.Unprojected
		return res, nil
	}
	if err := a.recoverProjections(ctx, tenantID, &res); err != nil {
		return res, err
	}
	for res.Leased < opts.MaxPairs {
		pairs, err := a.store.Lease(ctx, tenantID, min(leaseBatch, opts.MaxPairs-res.Leased), leaseFor, opts.Now())
		if err != nil {
			return res, fmt.Errorf("adjudicate artifacts: lease pairs: %w", err)
		}
		if len(pairs) == 0 {
			break
		}
		res.Leased += len(pairs)
		for _, p := range pairs {
			if err := a.adjudicatePair(ctx, tenantID, p, opts.Now, &res); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

// recoverProjections projects every decided verdict that is not yet in the
// graph, projectBatch at a time. Each projected verdict is marked, and one
// that a human takes over drops out of Unprojected, so the loop ends.
func (a *Adjudicator) recoverProjections(ctx context.Context, tenantID string, res *AdjudicateResult) error {
	for {
		batch, err := a.store.Unprojected(ctx, tenantID, projectBatch)
		if err != nil {
			return fmt.Errorf("adjudicate artifacts: load unprojected verdicts: %w", err)
		}
		for _, pr := range batch {
			if err := a.project(ctx, tenantID, pr, res); err != nil {
				return err
			}
			res.Recovered++
		}
		if len(batch) < projectBatch {
			return nil
		}
	}
}

func (a *Adjudicator) adjudicatePair(ctx context.Context, tenantID string, p LeasedPair, now func() time.Time, res *AdjudicateResult) error {
	if !now().Before(p.LeasedUntil) {
		res.Expired++
		return nil
	}
	v, err := a.panel(ctx, tenantID, p)
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
		return fmt.Errorf("adjudicate artifacts: pair %s: %w", p.Key, err)
	}
	v.DecidedAt = at
	return a.commit(ctx, tenantID, p, v, res)
}

// panel asks every model SamplesPerModel times whether p's groups denote one
// artifact and turns the votes into a verdict. A failed call or an
// unparseable reply is an errored vote; too many of them return an error
// wrapping *consensus.ErrInsufficientVotes, unless every call failed with a
// systemLLMErrors error, which returns errLLMUnavailable instead.
func (a *Adjudicator) panel(ctx context.Context, tenantID string, p LeasedPair) (Verdict, error) {
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
	eg.SetLimit(panelConcurrency)
	for i := range n {
		model, sample := a.cfg.Models[i/a.cfg.SamplesPerModel], i%a.cfg.SamplesPerModel
		eg.Go(func() error {
			voterID := fmt.Sprintf("%s__s%d", model, sample)
			resp, err := a.llm.Complete(ctx, &llmclient.CompletionRequest{
				Model: model, Messages: messages, MaxTokens: a.cfg.MaxTokens, Temperature: &temperature,
				TenantID: tenantID, PromptName: rp.Name, PromptVersion: rp.Version,
			})
			switch {
			case err != nil:
				votes[i], errs[i] = consensus.Vote{VoterID: voterID}, err
			case len(resp.Choices) == 0:
				votes[i], errs[i] = consensus.Vote{VoterID: voterID}, errors.New("completion returned no choices")
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
		return Verdict{}, panelError(err, votes, errs)
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
// run. Otherwise it adds the first completion error, named by its llmclient
// sentinel when it has one so the gateway's raw reply is not stored, or, when
// no call failed, how many replies had no valid SAME: line.
func panelError(err error, votes []consensus.Vote, errs []error) error {
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
	msg := first.Error()
	if i := slices.IndexFunc(llmErrors, func(s error) bool { return errors.Is(first, s) }); i >= 0 {
		msg = llmErrors[i].Error()
	}
	return fmt.Errorf("%w (first completion error: %s)", err, msg)
}

// promptValue makes an untrusted value safe to render inside a prompt tag:
// one line without format characters, at most maxPromptValueBytes of the
// original text, with no character that could close the tag. Escaping comes last, so the cap never
// splits an entity.
func promptValue(s string) string {
	s = promptBreaks.ReplaceAllString(promptInvisibles.ReplaceAllString(s, ""), " ")
	s = strings.TrimSpace(cleanText(s, maxPromptValueBytes))
	return promptEscaper.Replace(s)
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

// commit records v and, when the store applied it, projects it.
func (a *Adjudicator) commit(ctx context.Context, tenantID string, p LeasedPair, v Verdict, res *AdjudicateResult) error {
	outcome, err := a.store.Decide(ctx, tenantID, p.Key, v)
	if err != nil {
		return fmt.Errorf("adjudicate artifacts: record verdict for %s: %w", p.Key, err)
	}
	switch outcome {
	case DecideHumanOwned:
		if err := a.store.RecordDissent(ctx, tenantID, p.Key, v); err != nil {
			return fmt.Errorf("adjudicate artifacts: record dissent for %s: %w", p.Key, err)
		}
		res.Dissent++
		return nil
	case DecideStale:
		res.Stale++
		return nil
	}
	switch v.Status {
	case VerdictConfirmed:
		res.Confirmed++
	case VerdictRejected:
		res.Rejected++
	default:
		res.Undecided++
		return nil
	}
	return a.project(ctx, tenantID, Projection{
		Key: p.Key, LowGroupID: p.LowGroupID, HighGroupID: p.HighGroupID, CandidateEdgeID: p.CandidateEdgeID,
		Status: v.Status, Confidence: v.Confidence, Evidence: v.Evidence, DecidedAt: v.DecidedAt,
	}, res)
}

// project writes a decided verdict to the graph, then marks it projected. A
// confirmation adds an llm_panel SAME_AS edge (ErrEdgeExists means an earlier
// projection wrote it) and then supersedes the candidate, so a crash between
// the two leaves both live until the next run finishes the job. A rejection
// only supersedes the candidate. A confirmation whose groups are no longer in
// the graph has nothing to attach to: it counts as Orphaned and is marked
// projected, so it cannot block the queue.
func (a *Adjudicator) project(ctx context.Context, tenantID string, pr Projection, res *AdjudicateResult) error {
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
			return a.markProjected(ctx, tenantID, pr.Key)
		case err != nil && !errors.Is(err, graphdb.ErrEdgeExists):
			return fmt.Errorf("adjudicate artifacts: write SAME_AS for %s: %w", pr.Key, err)
		}
	}
	if _, err := a.g.SupersedeFact(ctx, tenantID, graphdb.SupersedeRequest{EdgeID: pr.CandidateEdgeID, SupersededAt: pr.DecidedAt}); err != nil {
		return fmt.Errorf("adjudicate artifacts: supersede %s: %w", pr.CandidateEdgeID, err)
	}
	return a.markProjected(ctx, tenantID, pr.Key)
}

func (a *Adjudicator) markProjected(ctx context.Context, tenantID, key string) error {
	if err := a.store.MarkProjected(ctx, tenantID, key); err != nil {
		return fmt.Errorf("adjudicate artifacts: mark %s projected: %w", key, err)
	}
	return nil
}

// recordFailure counts a failed panel and schedules the retry, or abandons
// the pair at max_attempts.
func (a *Adjudicator) recordFailure(ctx context.Context, tenantID string, p LeasedPair, cause error, at time.Time, res *AdjudicateResult) error {
	attempts := p.Attempts + 1
	f := Failure{Err: cleanText(cause.Error(), maxErrorChars), At: at, LeasedUntil: p.LeasedUntil,
		NextAttemptAt: at.Add(retryDelay(attempts)), Abandon: attempts >= a.cfg.MaxAttempts}
	if err := a.store.RecordFailure(ctx, tenantID, p.Key, f); err != nil {
		return fmt.Errorf("adjudicate artifacts: record failure for %s: %w", p.Key, err)
	}
	if f.Abandon {
		res.Abandoned++
	} else {
		res.Failed++
	}
	return nil
}

// retryDelay is the wait after the attempts-th failure: retryBaseDelay,
// doubling per failure, capped at retryMaxDelay.
func retryDelay(attempts int) time.Duration {
	d := retryBaseDelay
	for i := 1; i < attempts && d < retryMaxDelay; i++ {
		d *= 2
	}
	return min(d, retryMaxDelay)
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
