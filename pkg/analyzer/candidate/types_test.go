package candidate_test

import (
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
	"github.com/stretchr/testify/assert"
)

func TestCandidate_Structure(t *testing.T) {
	c := candidate.Candidate{
		SourceID:    "AC-1",
		TargetID:    "AC-2",
		Score:       0.85,
		Weight:      1.0,
		GeneratorID: "semantic",
		Metadata: map[string]string{
			"similarity": "0.85",
		},
	}

	assert.Equal(t, "AC-1", c.SourceID)
	assert.Equal(t, "AC-2", c.TargetID)
	assert.Equal(t, 0.85, c.Score)
	assert.Equal(t, 1.0, c.Weight)
	assert.Equal(t, "semantic", c.GeneratorID)
}

func TestControlData_Structure(t *testing.T) {
	cd := candidate.ControlData{
		ControlID: "AC-1",
		Text:      "Access control policy",
		Type:      "Procedural",
		Level:     "Strategic",
		Ancestor:  "Access Control",
	}

	assert.Equal(t, "AC-1", cd.ControlID)
	assert.Equal(t, "Procedural", cd.Type)
	assert.Equal(t, "Strategic", cd.Level)
}

func TestGenerateRequest_Structure(t *testing.T) {
	req := candidate.GenerateRequest{
		TenantID: "tenant-1",
		JobID:    "job-123",
		SourceControls: map[string]*candidate.ControlData{
			"AC-1": {ControlID: "AC-1"},
		},
		TargetControls: map[string]*candidate.ControlData{
			"AC-2": {ControlID: "AC-2"},
		},
		Parameters: map[string]interface{}{
			"top_k": 50,
		},
	}

	assert.Equal(t, "tenant-1", req.TenantID)
	assert.Equal(t, "job-123", req.JobID)
	assert.Len(t, req.SourceControls, 1)
	assert.Len(t, req.TargetControls, 1)
}
