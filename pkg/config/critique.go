package config

// CritiqueConfig configures the critique stage: a panel that re-reads each
// pair the relationship and requires panels accepted and vetoes the ones it
// judges wrong. It mirrors RequiresConfig's names and ranges. Global-only,
// like every analysis setting: AnalysisConfig has no tenant_overrides.
type CritiqueConfig struct {
	Enabled             bool     `yaml:"enabled" json:"enabled"`
	Models              []string `yaml:"models" json:"models"`                             // Panel aliases; must be non-empty when enabled and each a different alias
	SamplesPerModel     int      `yaml:"samples_per_model" json:"samples_per_model"`       // Votes per model per pair; must be positive and odd unless allow_even_samples=true
	AllowEvenSamples    bool     `yaml:"allow_even_samples" json:"allow_even_samples"`     // Allow even samples_per_model (default: false, requires odd for tie-breaking)
	SamplingTemperature float64  `yaml:"sampling_temperature" json:"sampling_temperature"` // Temperature for multi-sample; [0.0, 2.0]
	MaxTokens           int      `yaml:"max_tokens" json:"max_tokens"`                     // Max tokens for LLM response; must be positive
	ConsensusThreshold  float64  `yaml:"consensus_threshold" json:"consensus_threshold"`   // Minimum fraction of votes that must agree [0.5, 1.0]
	MaxErrorRate        float64  `yaml:"max_error_rate" json:"max_error_rate"`             // Maximum fraction of votes that can fail [0.0, 1.0]
	MaxSourceChars      int      `yaml:"max_source_chars" json:"max_source_chars"`         // Max runes for source control text; must be positive
	MaxTargetChars      int      `yaml:"max_target_chars" json:"max_target_chars"`         // Max runes for target control text; must be positive
	// AllowSharedPanelModels lets the panel aliases resolve to the same
	// upstream model, or the panel list a single alias. False (the default)
	// makes the stage fail when they do.
	// Set it for offline or single-model deployments. Global-only.
	AllowSharedPanelModels bool `yaml:"allow_shared_panel_models" json:"allow_shared_panel_models"`
	// Veto drops a verdict the critique rejects (REJECT share of valid votes
	// at or over ConsensusThreshold): it is then not a mapping and writes no
	// graph edge. False (the default) keeps the verdict and records the
	// critique outcome "rejected", so users see the critics' doubt and can
	// filter it with `results export --stage analyzer --exclude-rejected`.
	// Global-only.
	Veto bool `yaml:"veto" json:"veto"`
}

// Validate checks CritiqueConfig for consistency and required fields.
// Returns ErrInvalidConfig on validation failure.
func (c *CritiqueConfig) Validate() error { return c.validate(nil) }

func (c *CritiqueConfig) validate(tracker *sourceTracker) error {
	if !c.Enabled {
		return nil
	}
	return validatePanelSettings("analysis.critique", panelSettings{
		Models: c.Models, SamplesPerModel: c.SamplesPerModel, AllowEvenSamples: c.AllowEvenSamples,
		SamplingTemperature: c.SamplingTemperature, MaxTokens: c.MaxTokens,
		ConsensusThreshold: c.ConsensusThreshold, MaxErrorRate: c.MaxErrorRate,
		MaxSourceChars: c.MaxSourceChars, MaxTargetChars: c.MaxTargetChars,
	}, tracker)
}
