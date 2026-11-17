package cleaning

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	// maxStageRecordBytes bounds the stored JSON so a corrupt or hostile
	// column value cannot force a large allocation.
	maxStageRecordBytes = 64 * 1024
	// maxNameBytes bounds each panel alias and model revision.
	maxNameBytes = 256
)

// ErrInvalidStageRecord marks a stage record that is malformed, carries
// unknown fields, or exceeds the size limits. Callers should treat the stage
// as unrecorded and re-run it rather than guess.
var ErrInvalidStageRecord = errors.New("invalid stage record")

// StageRecord states which prompt and models produced a job stage, so a later
// run can tell whether the stage is stale.
type StageRecord struct {
	PromptName    string `json:"prompt_name"`
	PromptVersion string `json:"prompt_version"`
	// Models maps panel alias to its llm.model_revisions value; "" when unset.
	Models map[string]string `json:"models"`
}

func (r StageRecord) validate() error {
	for alias, revision := range r.Models {
		if len(alias) > maxNameBytes {
			return fmt.Errorf("%w: panel alias exceeds %d bytes", ErrInvalidStageRecord, maxNameBytes)
		}
		if len(revision) > maxNameBytes {
			return fmt.Errorf("%w: revision for alias %q exceeds %d bytes", ErrInvalidStageRecord, alias, maxNameBytes)
		}
	}
	return nil
}

// Marshal returns the canonical JSON form (fixed field order, sorted model
// keys). It rejects records ParseStageRecord would reject, so stored values
// always parse back.
func (r StageRecord) Marshal() (string, error) {
	if err := r.validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("marshal stage record: %w", err)
	}
	if len(b) > maxStageRecordBytes {
		return "", fmt.Errorf("%w: encoded record exceeds %d bytes", ErrInvalidStageRecord, maxStageRecordBytes)
	}
	return string(b), nil
}

// ParseStageRecord decodes a stored record. The empty string means the stage
// predates recording and yields (nil, nil): unknown, never stale. Unknown
// fields, trailing data, input over 64 KiB, and aliases or revisions over 256
// bytes wrap ErrInvalidStageRecord.
func ParseStageRecord(s string) (*StageRecord, error) {
	if s == "" {
		return nil, nil
	}
	if len(s) > maxStageRecordBytes {
		return nil, fmt.Errorf("%w: input exceeds %d bytes", ErrInvalidStageRecord, maxStageRecordBytes)
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.DisallowUnknownFields()
	var r StageRecord
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidStageRecord, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: unexpected data after the record", ErrInvalidStageRecord)
	}
	if err := r.validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// PromptStale reports whether the prompt version changed since the stage was
// recorded. An unknown version (nil record, or "" on either side) is never
// stale.
func PromptStale(recorded *StageRecord, current StageRecord) bool {
	return promptKnown(recorded, current) && recorded.PromptVersion != current.PromptVersion
}

// promptKnown reports whether both sides carry a prompt version to compare.
func promptKnown(recorded *StageRecord, current StageRecord) bool {
	return recorded != nil && recorded.PromptVersion != "" && current.PromptVersion != ""
}

// modelKnown reports whether the stage recorded its models. Revisions unset on
// one side are handled inside ModelStale, not as unknown.
func modelKnown(recorded *StageRecord, _ StageRecord) bool { return recorded != nil }

// ModelStale reports whether the model panel changed since the stage was
// recorded: any alias added or removed, or a revision that differs while set
// on both sides. A revision that is unset on either side is not a change, since
// the pin did not exist to compare. An unknown (nil) record is never stale.
func ModelStale(recorded *StageRecord, current StageRecord) bool {
	if recorded == nil || len(recorded.Models) != len(current.Models) {
		return recorded != nil
	}
	for alias, was := range recorded.Models {
		now, ok := current.Models[alias]
		if !ok {
			return true
		}
		if was != "" && now != "" && was != now {
			return true
		}
	}
	return false
}
