package analyzer

import (
	"math"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
)

// PayloadCacheRequest derives a task's cache request from its payload, the
// fields the worker builds the LLM request from: model, temperature (absent
// for embeddings), max_tokens, sample_index, and the messages (completion) or
// text (embedding) themselves. sourceField and targetField name the
// payload fields holding the controls whose text the request carries (the
// same field for a per-control task). Job-specific fields are never read.
//
// The identity's content hash is recomputed from the messages or text, never
// taken from the payload: the worker embeds a payload's content_hash only as
// a tamper check (and never checks it for embeddings), so a stale one would
// otherwise key one request under another's hash. A payload that carries a
// content_hash differing from the recomputed one is uncacheable (the worker
// rejects it too). A payload without one is cacheable: the worker accepts it
// and sends exactly those messages, so the recomputed hash identifies the
// request exactly.
//
// It returns ok=false for a pre-built task, a payload that is not a struct,
// messages the worker would reject, neither or both of messages and text, a
// stale content_hash, a missing control, or a token limit or sample that is
// not a non-negative whole number within int32.
func PayloadCacheRequest(task analyzer.Task, sourceField, targetField string) (analyzer.CacheRequest, bool) {
	if task.PreBuiltResult {
		return analyzer.CacheRequest{}, false
	}
	payload, ok := task.Payload.(*structpb.Struct)
	if !ok || payload == nil {
		return analyzer.CacheRequest{}, false
	}
	f := payload.GetFields()
	req := analyzer.CacheRequest{
		SourceControlID: f[sourceField].GetStringValue(),
		TargetControlID: f[targetField].GetStringValue(),
		Identity: resultcache.Identity{
			TaskType:   task.TaskType,
			ModelAlias: f["model"].GetStringValue(),
		},
	}
	var hashed bool
	if req.Identity.ContentHash, hashed = recomputeContentHash(f); !hashed {
		return analyzer.CacheRequest{}, false
	}
	if req.SourceControlID == "" || req.TargetControlID == "" {
		return analyzer.CacheRequest{}, false
	}
	var whole bool
	if req.Identity.MaxTokens, whole = wholeNumber(f["max_tokens"]); !whole {
		return analyzer.CacheRequest{}, false
	}
	if req.Identity.Sample, whole = wholeNumber(f["sample_index"]); !whole {
		return analyzer.CacheRequest{}, false
	}
	if v, present := f["temperature"]; present {
		t := v.GetNumberValue()
		req.Identity.Temperature = &t
	}
	return req, true
}

// wholeNumber converts a payload number to int64, refusing a value the
// conversion would change: fractional, negative, or over int32 (the worker
// narrows max_tokens to int). An absent field is 0.
func wholeNumber(v *structpb.Value) (int64, bool) {
	n := v.GetNumberValue()
	if n != math.Trunc(n) || n < 0 || n > math.MaxInt32 {
		return 0, false
	}
	return int64(n), true
}

// recomputeContentHash hashes the request the worker would send, decoding
// messages with llmclient.MessagesFromPayload, which the worker uses too,
// and text as internal/worker/payload.go does. ok=false when the
// worker would reject the payload or the payload's own content_hash differs.
func recomputeContentHash(f map[string]*structpb.Value) (string, bool) {
	msgValue, hasMessages := f["messages"]
	text := f["text"].GetStringValue()
	if hasMessages == (text != "") {
		return "", false
	}
	var hash string
	if hasMessages {
		messages, err := llmclient.MessagesFromPayload(msgValue)
		if err != nil {
			return "", false
		}
		hash = llmclient.ContentHash(messages)
	} else {
		hash = llmclient.ContentHash(text)
	}
	if claimed := f["content_hash"].GetStringValue(); claimed != "" && claimed != hash {
		return "", false
	}
	return hash, true
}
