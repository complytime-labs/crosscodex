package config_test

import (
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCandidateConfig_Defaults(t *testing.T) {
	cfg := config.CandidateConfig{}

	assert.Empty(t, cfg.Generators)
}

func TestCandidateGeneratorEntry_Structure(t *testing.T) {
	entry := config.CandidateGeneratorEntry{
		Name:    "semantic",
		Enabled: true,
		Weight:  1.5,
		Config: map[string]interface{}{
			"top_k": 10,
		},
	}

	assert.Equal(t, "semantic", entry.Name)
	assert.True(t, entry.Enabled)
	assert.Equal(t, 1.5, entry.Weight)
	assert.Equal(t, 10, entry.Config["top_k"])
}

func TestRequiresConfig_Structure(t *testing.T) {
	cfg := config.RequiresConfig{
		Enabled:             true,
		Models:              []string{"model-a", "model-b"},
		SamplesPerModel:     3,
		AllowEvenSamples:    false,
		SamplingTemperature: 0.7,
		MaxTokens:           500,
		ConsensusThreshold:  0.6,
		MaxErrorRate:        0.2,
		MaxSourceChars:      2000,
		MaxTargetChars:      2000,
	}

	assert.True(t, cfg.Enabled)
	assert.Equal(t, 3, cfg.SamplesPerModel)
	assert.Equal(t, 0.6, cfg.ConsensusThreshold)
}

func TestRequiresConfig_Validate_OddSamplesRequired(t *testing.T) {
	cfg := config.RequiresConfig{
		Enabled:             true,
		Models:              []string{"model-a"},
		SamplesPerModel:     4, // Even
		AllowEvenSamples:    false,
		ConsensusThreshold:  0.6,
		MaxErrorRate:        0.2,
		MaxSourceChars:      1000,
		MaxTargetChars:      1000,
		SamplingTemperature: 0.7,
		MaxTokens:           500,
	}

	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be odd")
}

func TestRequiresConfig_Validate_EvenSamplesAllowed(t *testing.T) {
	cfg := config.RequiresConfig{
		Enabled:             true,
		Models:              []string{"model-a"},
		SamplesPerModel:     4, // Even, but allowed
		AllowEvenSamples:    true,
		ConsensusThreshold:  0.6,
		MaxErrorRate:        0.2,
		MaxSourceChars:      1000,
		MaxTargetChars:      1000,
		SamplingTemperature: 0.7,
		MaxTokens:           500,
	}

	err := cfg.Validate()
	require.NoError(t, err)
}

func TestRequiresConfig_Validate_ConsensusThresholdRange(t *testing.T) {
	tests := []struct {
		name      string
		threshold float64
		wantErr   bool
	}{
		{"below_range", 0.4, true},
		{"min_valid", 0.5, false},
		{"mid_valid", 0.7, false},
		{"max_valid", 1.0, false},
		{"above_range", 1.1, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.RequiresConfig{
				Enabled:             true,
				Models:              []string{"model-a"},
				SamplesPerModel:     3,
				AllowEvenSamples:    false,
				ConsensusThreshold:  tt.threshold,
				MaxErrorRate:        0.2,
				MaxSourceChars:      1000,
				MaxTargetChars:      1000,
				SamplingTemperature: 0.7,
				MaxTokens:           500,
			}

			err := cfg.Validate()
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "consensus_threshold")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestRequiresConfig_Validate_MaxErrorRateRange(t *testing.T) {
	tests := []struct {
		name    string
		errRate float64
		wantErr bool
	}{
		{"below_range", -0.1, true},
		{"min_valid", 0.0, false},
		{"mid_valid", 0.5, false},
		{"max_valid", 1.0, false},
		{"above_range", 1.1, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.RequiresConfig{
				Enabled:             true,
				Models:              []string{"model-a"},
				SamplesPerModel:     3,
				AllowEvenSamples:    false,
				ConsensusThreshold:  0.6,
				MaxErrorRate:        tt.errRate,
				MaxSourceChars:      1000,
				MaxTargetChars:      1000,
				SamplingTemperature: 0.7,
				MaxTokens:           500,
			}

			err := cfg.Validate()
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "max_error_rate")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestRequiresConfig_Validate_RequiredFieldsWhenEnabled(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.RequiresConfig
		wantErr string
	}{
		{
			name: "empty_models",
			cfg: config.RequiresConfig{
				Enabled:             true,
				Models:              []string{},
				SamplesPerModel:     3,
				ConsensusThreshold:  0.6,
				MaxErrorRate:        0.2,
				MaxSourceChars:      1000,
				MaxTargetChars:      1000,
				SamplingTemperature: 0.7,
				MaxTokens:           500,
			},
			wantErr: "models must not be empty",
		},
		{
			name: "zero_samples_per_model",
			cfg: config.RequiresConfig{
				Enabled:             true,
				Models:              []string{"model-a"},
				SamplesPerModel:     0,
				ConsensusThreshold:  0.6,
				MaxErrorRate:        0.2,
				MaxSourceChars:      1000,
				MaxTargetChars:      1000,
				SamplingTemperature: 0.7,
				MaxTokens:           500,
			},
			wantErr: "must be positive",
		},
		{
			name: "zero_max_tokens",
			cfg: config.RequiresConfig{
				Enabled:             true,
				Models:              []string{"model-a"},
				SamplesPerModel:     3,
				ConsensusThreshold:  0.6,
				MaxErrorRate:        0.2,
				MaxSourceChars:      1000,
				MaxTargetChars:      1000,
				SamplingTemperature: 0.7,
				MaxTokens:           0,
			},
			wantErr: "must be positive",
		},
		{
			name: "zero_max_source_chars",
			cfg: config.RequiresConfig{
				Enabled:             true,
				Models:              []string{"model-a"},
				SamplesPerModel:     3,
				ConsensusThreshold:  0.6,
				MaxErrorRate:        0.2,
				MaxSourceChars:      0,
				MaxTargetChars:      1000,
				SamplingTemperature: 0.7,
				MaxTokens:           500,
			},
			wantErr: "must be positive",
		},
		{
			name: "zero_max_target_chars",
			cfg: config.RequiresConfig{
				Enabled:             true,
				Models:              []string{"model-a"},
				SamplesPerModel:     3,
				ConsensusThreshold:  0.6,
				MaxErrorRate:        0.2,
				MaxSourceChars:      1000,
				MaxTargetChars:      0,
				SamplingTemperature: 0.7,
				MaxTokens:           500,
			},
			wantErr: "must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestRequiresConfig_Validate_DisabledSkipsValidation(t *testing.T) {
	// Invalid config, but disabled so should pass
	cfg := config.RequiresConfig{
		Enabled:            false,
		Models:             []string{}, // Empty, but OK when disabled
		SamplesPerModel:    0,
		ConsensusThreshold: 2.0, // Invalid, but OK when disabled
		MaxErrorRate:       -1.0,
	}

	err := cfg.Validate()
	require.NoError(t, err)
}

func TestRequiresConfig_Validate_SamplingTemperatureRange(t *testing.T) {
	tests := []struct {
		name    string
		temp    float64
		wantErr bool
	}{
		{"below_range", -0.1, true},
		{"min_valid", 0.0, false},
		{"mid_valid", 1.0, false},
		{"max_valid", 2.0, false},
		{"above_range", 2.1, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.RequiresConfig{
				Enabled:             true,
				Models:              []string{"model-a"},
				SamplesPerModel:     3,
				ConsensusThreshold:  0.6,
				MaxErrorRate:        0.2,
				MaxSourceChars:      1000,
				MaxTargetChars:      1000,
				SamplingTemperature: tt.temp,
				MaxTokens:           500,
			}

			err := cfg.Validate()
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "sampling_temperature")
			} else {
				require.NoError(t, err)
			}
		})
	}
}
