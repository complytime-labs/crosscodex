package main

import (
	"fmt"
	"io"
	"strings"

	"golang.org/x/text/language"
	"golang.org/x/text/message"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	intanalyzer "github.com/complytime-labs/crosscodex/internal/analyzer"
)

// counts formats counts with thousands separators ("1,190").
var counts = message.NewPrinter(language.English)

// unattestedCacheLine is the line of an analyzer no signature vouches for.
const unattestedCacheLine = "cache use not attested: no signed checkpoint vouches for this stage's records"

// cacheLine describes one analyzer's result cache use, after its name. Only
// a summary the server marks attested shows a mode and counts.
func cacheLine(s *pb.AnalyzerCacheSummary) string {
	switch s.GetAttestation() {
	case pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_IN_PROGRESS:
		return "in progress"
	case pb.ResultCacheAttestation_RESULT_CACHE_ATTESTATION_ATTESTED:
	default:
		return unattestedCacheLine
	}
	reason := intanalyzer.CleanDisplay(s.GetReason(), maxLookupErrorBytes)
	switch s.GetMode() {
	case pb.ResultCacheMode_RESULT_CACHE_MODE_DISABLED:
		return "cache disabled: " + reason
	case pb.ResultCacheMode_RESULT_CACHE_MODE_SKIPPED:
		return "not cached: " + reason
	case pb.ResultCacheMode_RESULT_CACHE_MODE_UNRECORDED:
		return "cache use not recorded for this stage (signed as unrecorded): " + reason
	case pb.ResultCacheMode_RESULT_CACHE_MODE_BYPASSED:
		return counts.Sprintf("cache bypassed (--no-cache): %d of %d sent to %s; results stored for later jobs",
			s.GetDispatched(), s.GetTasks(), cacheModels(s.GetModels()))
	default:
		line := counts.Sprintf("%d of %d reused%s, %d sent to %s",
			s.GetHits(), s.GetTasks(), cacheProducers(s), s.GetDispatched(), cacheModels(s.GetModels()))
		if reason != "" {
			line += "; " + reason
		}
		return line
	}
}

// cacheProducers names the earlier jobs whose results were reused: the
// caller's by ID, other users' only by count.
func cacheProducers(s *pb.AnalyzerCacheSummary) string {
	owned := make([]string, len(s.GetProducerJobIds()))
	for i, id := range s.GetProducerJobIds() {
		owned[i] = intanalyzer.CleanDisplay(id, maxUntrustedDisplayBytes)
	}
	others := s.GetOtherProducerJobs()
	total := int64(len(owned)) + others
	if total == 0 {
		return ""
	}
	noun := "jobs"
	if total == 1 {
		noun = "job"
	}
	switch {
	case len(owned) == 0:
		return counts.Sprintf(" (from %d earlier %s by other users)", total, noun)
	case others == 0:
		return counts.Sprintf(" (from %d earlier %s: %s)", total, noun, strings.Join(owned, ", "))
	default:
		return counts.Sprintf(" (from %d earlier %s: %s and %d by other users)", total, noun, strings.Join(owned, ", "), others)
	}
}

// cacheModels names each alias and the model the gateway reported for it,
// with the first 8 characters of its deployment ID (enough to tell two
// deployments apart; --json has the whole ID) and its revision.
func cacheModels(models []*pb.AnalyzerCacheModel) string {
	if len(models) == 0 {
		return "the model"
	}
	parts := make([]string, len(models))
	for i, m := range models {
		alias := intanalyzer.CleanDisplay(m.GetAlias(), maxUntrustedDisplayBytes)
		if alias == "" {
			alias = "(no model configured)"
		}
		observed := intanalyzer.CleanDisplay(m.GetObservedModel(), maxUntrustedDisplayBytes)
		if observed == "" {
			observed = "unknown"
		}
		parts[i] = alias + " → " + observed
		if d := []rune(intanalyzer.CleanDisplay(m.GetDeployment(), maxUntrustedDisplayBytes)); len(d) > 0 {
			parts[i] += " (deployment " + string(d[:min(len(d), 8)]) + ")"
		}
		if r := intanalyzer.CleanDisplay(m.GetRevision(), maxUntrustedDisplayBytes); r != "" {
			parts[i] += " (revision " + r + ")"
		}
	}
	noun := "model"
	if len(models) > 1 {
		noun = "models"
	}
	return noun + " " + strings.Join(parts, ", ")
}

// writeCacheLines prints the "Result cache:" section, analyzer names
// aligned; nothing when there are no summaries. In embedded mode, which
// has no attestation keys and shows no summaries, it says so for a job
// with analyzers instead.
func writeCacheLines(w io.Writer, sums []*pb.AnalyzerCacheSummary, embedded bool, analyzers int) {
	if len(sums) == 0 {
		if embedded && analyzers > 0 {
			fmt.Fprintf(w, "Result cache: shown only when connected to crosscodexd\n")
		}
		return
	}
	width := 0
	names := make([]string, len(sums))
	for i, s := range sums {
		names[i] = intanalyzer.CleanDisplay(s.GetAnalyzer(), maxUntrustedDisplayBytes)
		width = max(width, len(names[i]))
	}
	fmt.Fprintf(w, "Result cache:\n")
	for i, s := range sums {
		fmt.Fprintf(w, "  %-*s  %s\n", width, names[i], cacheLine(s))
	}
}

type cacheModelJSON struct {
	Alias         string `json:"alias"`
	ObservedModel string `json:"observed_model"`
	Deployment    string `json:"deployment"`
	Revision      string `json:"revision"`
}

type cacheSummaryJSON struct {
	Analyzer          string           `json:"analyzer"`
	Attestation       string           `json:"attestation"`
	Mode              string           `json:"mode"`
	Reason            string           `json:"reason"`
	Tasks             int64            `json:"tasks"`
	Hits              int64            `json:"hits"`
	Dispatched        int64            `json:"dispatched"`
	Models            []cacheModelJSON `json:"models"`
	ProducerJobIDs    []string         `json:"producer_job_ids"`
	OtherProducerJobs int64            `json:"other_producer_jobs"`
}

// cacheToJSON is run status --json's "cache": snake_case keys, the
// attestation ("attested", "unattested" or "in_progress") and the mode
// ("enabled", "bypassed", "disabled", "skipped" or "unrecorded", or ""
// when the summary has none) as words, never null.
func cacheToJSON(sums []*pb.AnalyzerCacheSummary) []cacheSummaryJSON {
	out := make([]cacheSummaryJSON, 0, len(sums))
	for _, s := range sums {
		models := make([]cacheModelJSON, 0, len(s.GetModels()))
		for _, m := range s.GetModels() {
			models = append(models, cacheModelJSON{Alias: m.GetAlias(), ObservedModel: m.GetObservedModel(),
				Deployment: m.GetDeployment(), Revision: m.GetRevision()})
		}
		producers := s.GetProducerJobIds()
		if producers == nil {
			producers = []string{}
		}
		mode := ""
		if s.GetMode() != pb.ResultCacheMode_RESULT_CACHE_MODE_UNSPECIFIED {
			mode = strings.ToLower(strings.TrimPrefix(s.GetMode().String(), "RESULT_CACHE_MODE_"))
		}
		out = append(out, cacheSummaryJSON{
			Analyzer:    s.GetAnalyzer(),
			Attestation: strings.ToLower(strings.TrimPrefix(s.GetAttestation().String(), "RESULT_CACHE_ATTESTATION_")),
			Mode:        mode,
			Reason:      s.GetReason(), Tasks: s.GetTasks(), Hits: s.GetHits(), Dispatched: s.GetDispatched(),
			Models: models, ProducerJobIDs: producers, OtherProducerJobs: s.GetOtherProducerJobs(),
		})
	}
	return out
}
