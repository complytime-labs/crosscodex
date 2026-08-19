package analyzer

import (
	"regexp"
	"strconv"
	"strings"
)

var reSampleSuffix = regexp.MustCompile(`-s(\d+)$`)

// StripTaskIDPrefix removes "<analyzerName>-" from the front of taskID.
// Returns ok=false if taskID does not start with that prefix.
func StripTaskIDPrefix(taskID, analyzerName string) (rest string, ok bool) {
	prefix := analyzerName + "-"
	if !strings.HasPrefix(taskID, prefix) {
		return "", false
	}
	return taskID[len(prefix):], true
}

// ParsePairTaskID reverses the TaskID format GenerateWork uses for
// pairwise analyzers (relationship, requires):
// "<analyzerName>-<source>--<target>-<model>-s<N>".
//
// Both control IDs and model names may themselves contain hyphens, so the
// target/model boundary is disambiguated against the analyzer's own
// configured model list rather than parsed positionally. If no configured
// model matches as a suffix, ok=false — callers should skip the result
// rather than guess.
func ParsePairTaskID(taskID, analyzerName string, models []string) (source, target, model string, sample int, ok bool) {
	rest, ok := StripTaskIDPrefix(taskID, analyzerName)
	if !ok {
		return "", "", "", 0, false
	}

	idx := strings.Index(rest, "--")
	if idx < 0 {
		return "", "", "", 0, false
	}
	source = rest[:idx]
	tail := rest[idx+2:]

	sm := reSampleSuffix.FindStringSubmatch(tail)
	if sm == nil {
		return "", "", "", 0, false
	}
	sampleN, err := strconv.Atoi(sm[1])
	if err != nil {
		return "", "", "", 0, false
	}
	targetModel := strings.TrimSuffix(tail, sm[0])

	for _, m := range models {
		if strings.HasSuffix(targetModel, "-"+m) {
			return source, strings.TrimSuffix(targetModel, "-"+m), m, sampleN, true
		}
	}
	return "", "", "", 0, false
}

// ParseControlTaskID reverses the TaskID formats GenerateWork uses for
// per-control analyzers (artifacts):
// "<analyzerName>-<controlID>-<model>-s<N>" for LLM-dispatched tasks, or
// "<analyzerName>-<controlID>-skip" for auto-skipped sections.
func ParseControlTaskID(taskID, analyzerName string, models []string) (controlID, model string, sample int, skipped, ok bool) {
	rest, ok := StripTaskIDPrefix(taskID, analyzerName)
	if !ok {
		return "", "", 0, false, false
	}

	if cid, found := strings.CutSuffix(rest, "-skip"); found {
		return cid, "", 0, true, true
	}

	sm := reSampleSuffix.FindStringSubmatch(rest)
	if sm == nil {
		return "", "", 0, false, false
	}
	sampleN, err := strconv.Atoi(sm[1])
	if err != nil {
		return "", "", 0, false, false
	}
	controlModel := strings.TrimSuffix(rest, sm[0])

	for _, m := range models {
		if strings.HasSuffix(controlModel, "-"+m) {
			return strings.TrimSuffix(controlModel, "-"+m), m, sampleN, false, true
		}
	}
	return "", "", 0, false, false
}
