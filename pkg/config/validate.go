package config

import (
	"fmt"
	"regexp"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var (
	validTLSModes        = map[string]bool{"off": true, "server-only": true, "mutual": true}
	validStorageBackends = map[string]bool{"local": true, "s3": true}
	validLogLevels       = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	validLogFormats      = map[string]bool{"text": true, "json": true}

	// validRelationshipTypes is the single source of truth for NIST IR 8477
	// relationship type names accepted in config. Mirrors the 8 enum values
	// in synthesis.proto and internal/analyzer/relationship/types.go.
	validRelationshipTypes = map[string]bool{
		"EQUIVALENT": true, "SUPERSET_OF": true, "SUBSET_OF": true,
		"CONTRIBUTES_TO": true, "COMPLEMENTS": true, "PARTIAL": true,
		"CONFLICTS_WITH": true, "NO_RELATIONSHIP": true,
	}
)

func validate(cfg *Config, tracker *sourceTracker) error {
	if err := validateTLS(&cfg.TLS, tracker); err != nil {
		return err
	}
	if err := validateStorage(&cfg.Storage, tracker); err != nil {
		return err
	}
	if err := validateLogging(&cfg.Logging, tracker); err != nil {
		return err
	}
	if err := validateLLM(&cfg.LLM, tracker); err != nil {
		return err
	}
	if err := validateCatalog(&cfg.Catalog, tracker); err != nil {
		return err
	}
	if err := validateAttestation(&cfg.Attestation, tracker); err != nil {
		return err
	}
	if err := validatePrompt(&cfg.Prompt, tracker); err != nil {
		return err
	}
	if err := validateAnalysis(&cfg.Analysis, tracker); err != nil {
		return err
	}
	if err := cfg.Worker.Validate(); err != nil {
		return err
	}
	if err := validateSynthesis(&cfg.Synthesis, tracker); err != nil {
		return err
	}
	return nil
}

func validateTLS(tls *TLSConfig, tracker *sourceTracker) error {
	if tls.Mode != "" && !validTLSModes[tls.Mode] {
		return fmt.Errorf("tls.mode %q%s must be one of off, server-only, mutual: %w",
			tls.Mode, formatSource(tracker, "tls.mode"), ErrInvalidConfig)
	}

	switch tls.Mode {
	case "mutual":
		if tls.Cert == "" || tls.Key == "" || tls.CA == "" {
			return fmt.Errorf("tls.mode mutual%s requires cert, key, and ca: %w",
				formatSource(tracker, "tls.mode"), ErrInvalidConfig)
		}
	case "server-only":
		if tls.Cert == "" || tls.Key == "" {
			return fmt.Errorf("tls.mode server-only%s requires cert and key: %w",
				formatSource(tracker, "tls.mode"), ErrInvalidConfig)
		}
	}

	// Validate per-target overrides
	for name, override := range tls.Targets {
		if override.Mode != "" && !validTLSModes[override.Mode] {
			return fmt.Errorf("tls.targets.%s.mode %q must be one of off, server-only, mutual: %w",
				name, override.Mode, ErrInvalidConfig)
		}
	}

	return nil
}

func validateStorage(s *StorageConfig, tracker *sourceTracker) error {
	if s.Objects.Backend != "" && !validStorageBackends[s.Objects.Backend] {
		return fmt.Errorf("storage.objects.backend %q%s must be one of local, s3: %w",
			s.Objects.Backend, formatSource(tracker, "storage.objects.backend"), ErrInvalidConfig)
	}
	return nil
}

func validateLogging(l *LoggingConfig, tracker *sourceTracker) error {
	if l.Level != "" && !validLogLevels[l.Level] {
		return fmt.Errorf("logging.level %q%s must be one of debug, info, warn, error: %w",
			l.Level, formatSource(tracker, "logging.level"), ErrInvalidConfig)
	}
	if l.Format != "" && !validLogFormats[l.Format] {
		return fmt.Errorf("logging.format %q%s must be one of text, json: %w",
			l.Format, formatSource(tracker, "logging.format"), ErrInvalidConfig)
	}
	return nil
}

func validateLLM(llm *LLMConfig, tracker *sourceTracker) error {
	if llm.Timeout < 0 {
		return fmt.Errorf("llm.timeout %d%s must be non-negative: %w",
			llm.Timeout, formatSource(tracker, "llm.timeout"), ErrInvalidConfig)
	}
	if llm.MaxRetries < 0 {
		return fmt.Errorf("llm.max_retries %d%s must be non-negative: %w",
			llm.MaxRetries, formatSource(tracker, "llm.max_retries"), ErrInvalidConfig)
	}
	if llm.GatewayMode && llm.GatewayURL == "" {
		return fmt.Errorf(
			"llm.gateway_url%s must be set when llm.gateway_mode is true; "+
				"set gateway_url to your LLM proxy address (e.g. http://litellm:4000): %w",
			formatSource(tracker, "llm.gateway_url"), ErrInvalidConfig)
	}
	// When a gateway handles retries, client-side retries are redundant.
	// Normalize to zero so llmclient does not need its own check.
	if llm.GatewayMode && llm.MaxRetries > 0 {
		llm.MaxRetries = 0
	}
	for key, override := range llm.TenantOverrides {
		if err := tenant.ValidateTenantID(key); err != nil {
			return fmt.Errorf("llm.tenant_overrides key %q is not a valid tenant ID: %w",
				key, ErrInvalidConfig)
		}
		if override.MaxRetries != nil && *override.MaxRetries < 0 {
			return fmt.Errorf("llm.tenant_overrides.%s.max_retries %d must be non-negative: %w",
				key, *override.MaxRetries, ErrInvalidConfig)
		}
		if override.Timeout != nil && *override.Timeout < 0 {
			return fmt.Errorf("llm.tenant_overrides.%s.timeout %d must be non-negative: %w",
				key, *override.Timeout, ErrInvalidConfig)
		}
	}
	return nil
}

func validateAttestation(a *AttestationConfig, tracker *sourceTracker) error {
	if a.ExpiryDuration <= 0 {
		return fmt.Errorf("attestation.expiry_duration %s%s must be positive: %w",
			a.ExpiryDuration, formatSource(tracker, "attestation.expiry_duration"), ErrInvalidConfig)
	}
	if (a.PrivateKeyPath == "") != (a.PublicKeyPath == "") {
		return fmt.Errorf("attestation.private_key_path and attestation.public_key_path%s must both be set or both empty: %w",
			formatSource(tracker, "attestation.private_key_path"), ErrInvalidConfig)
	}
	for tenantID, override := range a.TenantOverrides {
		if err := tenant.ValidateTenantID(tenantID); err != nil {
			return fmt.Errorf("attestation.tenant_overrides: invalid tenant ID %q: %w", tenantID, ErrInvalidConfig)
		}
		if override.ExpiryDuration != nil && *override.ExpiryDuration <= 0 {
			return fmt.Errorf("attestation.tenant_overrides.%s.expiry_duration %s must be positive: %w",
				tenantID, *override.ExpiryDuration, ErrInvalidConfig)
		}
		privSet := override.PrivateKeyPath != nil && *override.PrivateKeyPath != ""
		pubSet := override.PublicKeyPath != nil && *override.PublicKeyPath != ""
		if privSet != pubSet {
			return fmt.Errorf("attestation.tenant_overrides.%s.private_key_path and public_key_path must both be set or both empty: %w",
				tenantID, ErrInvalidConfig)
		}
	}
	return nil
}

func validateCatalog(cat *CatalogConfig, tracker *sourceTracker) error {
	s := &cat.Structuring
	if s.MinDecomposeWords < 0 {
		return fmt.Errorf("catalog.structuring.min_decompose_words %d%s must be non-negative: %w",
			s.MinDecomposeWords, formatSource(tracker, "catalog.structuring.min_decompose_words"), ErrInvalidConfig)
	}
	if s.ChunkChars < 0 {
		return fmt.Errorf("catalog.structuring.chunk_chars %d%s must be non-negative: %w",
			s.ChunkChars, formatSource(tracker, "catalog.structuring.chunk_chars"), ErrInvalidConfig)
	}
	if s.MaxValidationChars < 0 {
		return fmt.Errorf("catalog.structuring.max_validation_chars %d%s must be non-negative: %w",
			s.MaxValidationChars, formatSource(tracker, "catalog.structuring.max_validation_chars"), ErrInvalidConfig)
	}
	if s.MaxHeadingRepeats < 0 {
		return fmt.Errorf("catalog.structuring.max_heading_repeats %d%s must be non-negative: %w",
			s.MaxHeadingRepeats, formatSource(tracker, "catalog.structuring.max_heading_repeats"), ErrInvalidConfig)
	}
	if s.SectionPattern != "" {
		if _, err := regexp.Compile(s.SectionPattern); err != nil {
			return fmt.Errorf("catalog.structuring.section_pattern %q%s is not valid regex: %w",
				s.SectionPattern, formatSource(tracker, "catalog.structuring.section_pattern"), ErrInvalidConfig)
		}
	}
	return nil
}

var validMergeModes = map[string]bool{
	"":        true, // empty defaults to "merge"
	"merge":   true,
	"replace": true,
}

var validSliceStrategies = map[string]bool{
	"":          true, // empty defaults to "replace"
	"replace":   true,
	"append":    true,
	"deep_copy": true,
}

var validLayerIDs = map[string]bool{
	"embedded": true,
	"user":     true,
	"project":  true,
	"cli":      true,
}

func validatePrompt(prompt *PromptConfig, tracker *sourceTracker) error {
	seen := make(map[string]bool)
	for i, entry := range prompt.Layers.Order {
		// Validate layer ID is a known built-in or a layer_paths entry
		if !validLayerIDs[entry.ID] {
			found := false
			for _, lp := range prompt.LayerPaths {
				if lp == entry.ID {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("prompt.layers.order[%d].id %q%s must be one of embedded, user, project, cli or a path from layer_paths: %w",
					i, entry.ID, formatSource(tracker, "prompt.layers.order"), ErrInvalidConfig)
			}
		}

		// Check for duplicates
		if seen[entry.ID] {
			return fmt.Errorf("prompt.layers.order[%d].id %q%s is a duplicate layer ID: %w",
				i, entry.ID, formatSource(tracker, "prompt.layers.order"), ErrInvalidConfig)
		}
		seen[entry.ID] = true

		// Validate merge mode
		if !validMergeModes[entry.Merge] {
			return fmt.Errorf("prompt.layers.order[%d].merge %q%s must be one of merge, replace: %w",
				i, entry.Merge, formatSource(tracker, "prompt.layers.order"), ErrInvalidConfig)
		}

		// Validate slice strategy
		if !validSliceStrategies[entry.SliceStrategy] {
			return fmt.Errorf("prompt.layers.order[%d].slice_strategy %q%s must be one of replace, append, deep_copy: %w",
				i, entry.SliceStrategy, formatSource(tracker, "prompt.layers.order"), ErrInvalidConfig)
		}
	}

	return nil
}

func validateAnalysis(a *AnalysisConfig, tracker *sourceTracker) error {
	c := &a.Classification
	if c.MaxTextLength <= 0 {
		return fmt.Errorf("analysis.classification.max_text_length %d%s must be positive: %w",
			c.MaxTextLength, formatSource(tracker, "analysis.classification.max_text_length"), ErrInvalidConfig)
	}
	if c.MaxTokens <= 0 {
		return fmt.Errorf("analysis.classification.max_tokens %d%s must be positive: %w",
			c.MaxTokens, formatSource(tracker, "analysis.classification.max_tokens"), ErrInvalidConfig)
	}
	if c.Temperature < 0 {
		return fmt.Errorf("analysis.classification.temperature %g%s must be non-negative: %w",
			c.Temperature, formatSource(tracker, "analysis.classification.temperature"), ErrInvalidConfig)
	}
	if c.Temperature > 2.0 {
		return fmt.Errorf("analysis.classification.temperature %g%s must not exceed 2.0: %w",
			c.Temperature, formatSource(tracker, "analysis.classification.temperature"), ErrInvalidConfig)
	}
	e := &a.Embedding
	if e.MaxChars < 0 {
		return fmt.Errorf("analysis.embedding.max_chars %d%s must be non-negative: %w",
			e.MaxChars, formatSource(tracker, "analysis.embedding.max_chars"), ErrInvalidConfig)
	}
	if e.BatchSize <= 0 {
		return fmt.Errorf("analysis.embedding.batch_size %d%s must be positive: %w",
			e.BatchSize, formatSource(tracker, "analysis.embedding.batch_size"), ErrInvalidConfig)
	}
	if e.Enabled && len(e.Models) == 0 {
		return fmt.Errorf("analysis.embedding.models%s must not be empty when enabled: %w",
			formatSource(tracker, "analysis.embedding.models"), ErrInvalidConfig)
	}
	r := &a.Relationship
	if r.TopK <= 0 {
		return fmt.Errorf("analysis.relationship.top_k %d%s must be positive: %w",
			r.TopK, formatSource(tracker, "analysis.relationship.top_k"), ErrInvalidConfig)
	}
	if r.MaxSourceChars <= 0 {
		return fmt.Errorf("analysis.relationship.max_source_chars %d%s must be positive: %w",
			r.MaxSourceChars, formatSource(tracker, "analysis.relationship.max_source_chars"), ErrInvalidConfig)
	}
	if r.MaxTargetChars <= 0 {
		return fmt.Errorf("analysis.relationship.max_target_chars %d%s must be positive: %w",
			r.MaxTargetChars, formatSource(tracker, "analysis.relationship.max_target_chars"), ErrInvalidConfig)
	}
	if r.MaxTokens <= 0 {
		return fmt.Errorf("analysis.relationship.max_tokens %d%s must be positive: %w",
			r.MaxTokens, formatSource(tracker, "analysis.relationship.max_tokens"), ErrInvalidConfig)
	}
	if r.SamplesPerModel <= 0 {
		return fmt.Errorf("analysis.relationship.samples_per_model %d%s must be positive: %w",
			r.SamplesPerModel, formatSource(tracker, "analysis.relationship.samples_per_model"), ErrInvalidConfig)
	}
	if r.SamplingTemperature < 0 {
		return fmt.Errorf("analysis.relationship.sampling_temperature %g%s must be non-negative: %w",
			r.SamplingTemperature, formatSource(tracker, "analysis.relationship.sampling_temperature"), ErrInvalidConfig)
	}
	if r.SamplingTemperature > 2.0 {
		return fmt.Errorf("analysis.relationship.sampling_temperature %g%s must not exceed 2.0: %w",
			r.SamplingTemperature, formatSource(tracker, "analysis.relationship.sampling_temperature"), ErrInvalidConfig)
	}
	if r.Enabled && len(r.Models) == 0 {
		return fmt.Errorf("analysis.relationship.models%s must not be empty when enabled: %w",
			formatSource(tracker, "analysis.relationship.models"), ErrInvalidConfig)
	}
	for _, at := range r.ActionableTypes {
		if !validRelationshipTypes[at] {
			return fmt.Errorf("analysis.relationship.actionable_types: invalid type %q%s: %w",
				at, formatSource(tracker, "analysis.relationship.actionable_types"), ErrInvalidConfig)
		}
	}

	// Validate engine config
	if err := a.Engine.Validate(); err != nil {
		return err
	}

	// Validate requires config
	if err := a.Requires.Validate(); err != nil {
		return err
	}

	if err := a.Artifacts.Validate(); err != nil {
		return err
	}

	return nil
}

func validateSynthesis(s *SynthesisConfig, tracker *sourceTracker) error {
	// Viability factors: must be > 0 and <= 2
	if s.Viability.TypeMismatchFactor <= 0 || s.Viability.TypeMismatchFactor > 2 {
		return fmt.Errorf("synthesis.viability.type_mismatch_factor %g%s must be in range (0, 2]: %w",
			s.Viability.TypeMismatchFactor, formatSource(tracker, "synthesis.viability.type_mismatch_factor"), ErrInvalidConfig)
	}
	if s.Viability.SkipLevelFactor <= 0 || s.Viability.SkipLevelFactor > 2 {
		return fmt.Errorf("synthesis.viability.skip_level_factor %g%s must be in range (0, 2]: %w",
			s.Viability.SkipLevelFactor, formatSource(tracker, "synthesis.viability.skip_level_factor"), ErrInvalidConfig)
	}
	if s.Viability.IntegralToFactor <= 0 || s.Viability.IntegralToFactor > 2 {
		return fmt.Errorf("synthesis.viability.integral_to_factor %g%s must be in range (0, 2]: %w",
			s.Viability.IntegralToFactor, formatSource(tracker, "synthesis.viability.integral_to_factor"), ErrInvalidConfig)
	}

	// Assessment: IQRGood > IQRPoor, both > 0
	if s.Assessment.IQRGood <= 0 {
		return fmt.Errorf("synthesis.assessment.iqr_good %g%s must be positive: %w",
			s.Assessment.IQRGood, formatSource(tracker, "synthesis.assessment.iqr_good"), ErrInvalidConfig)
	}
	if s.Assessment.IQRPoor <= 0 {
		return fmt.Errorf("synthesis.assessment.iqr_poor %g%s must be positive: %w",
			s.Assessment.IQRPoor, formatSource(tracker, "synthesis.assessment.iqr_poor"), ErrInvalidConfig)
	}
	if s.Assessment.IQRGood <= s.Assessment.IQRPoor {
		return fmt.Errorf("synthesis.assessment.iqr_good %g%s must be greater than iqr_poor %g: %w",
			s.Assessment.IQRGood, formatSource(tracker, "synthesis.assessment.iqr_good"), s.Assessment.IQRPoor, ErrInvalidConfig)
	}

	// Fraction thresholds: must be in [0, 1]
	if s.Assessment.NoRelHigh < 0 || s.Assessment.NoRelHigh > 1 {
		return fmt.Errorf("synthesis.assessment.no_rel_high %g%s must be in range [0, 1]: %w",
			s.Assessment.NoRelHigh, formatSource(tracker, "synthesis.assessment.no_rel_high"), ErrInvalidConfig)
	}
	if s.Assessment.NoRelLow < 0 || s.Assessment.NoRelLow > 1 {
		return fmt.Errorf("synthesis.assessment.no_rel_low %g%s must be in range [0, 1]: %w",
			s.Assessment.NoRelLow, formatSource(tracker, "synthesis.assessment.no_rel_low"), ErrInvalidConfig)
	}
	if s.Assessment.NoRelHigh <= s.Assessment.NoRelLow {
		return fmt.Errorf("synthesis.assessment.no_rel_high %g%s must be greater than no_rel_low %g: %w",
			s.Assessment.NoRelHigh, formatSource(tracker, "synthesis.assessment.no_rel_high"), s.Assessment.NoRelLow, ErrInvalidConfig)
	}
	if s.Assessment.ContestedWarn < 0 || s.Assessment.ContestedWarn > 1 {
		return fmt.Errorf("synthesis.assessment.contested_warn %g%s must be in range [0, 1]: %w",
			s.Assessment.ContestedWarn, formatSource(tracker, "synthesis.assessment.contested_warn"), ErrInvalidConfig)
	}
	if s.Assessment.ActionableWarn < 0 || s.Assessment.ActionableWarn > 1 {
		return fmt.Errorf("synthesis.assessment.actionable_warn %g%s must be in range [0, 1]: %w",
			s.Assessment.ActionableWarn, formatSource(tracker, "synthesis.assessment.actionable_warn"), ErrInvalidConfig)
	}

	// Global confidence threshold and max mappings
	if s.ConfidenceThreshold < 0 || s.ConfidenceThreshold > 1 {
		return fmt.Errorf("synthesis.confidence_threshold %g%s must be in range [0, 1]: %w",
			s.ConfidenceThreshold, formatSource(tracker, "synthesis.confidence_threshold"), ErrInvalidConfig)
	}
	if s.MaxMappingsPerControl <= 0 {
		return fmt.Errorf("synthesis.max_mappings_per_control %d%s must be positive: %w",
			s.MaxMappingsPerControl, formatSource(tracker, "synthesis.max_mappings_per_control"), ErrInvalidConfig)
	}

	// Validate tenant overrides
	for key, override := range s.TenantOverrides {
		if err := tenant.ValidateTenantID(key); err != nil {
			return fmt.Errorf("synthesis.tenant_overrides key %q is not a valid tenant ID: %w",
				key, ErrInvalidConfig)
		}
		if override.ConfidenceThreshold != nil {
			if *override.ConfidenceThreshold < 0 || *override.ConfidenceThreshold > 1 {
				return fmt.Errorf("synthesis.tenant_overrides.%s.confidence_threshold %g must be in range [0, 1]: %w",
					key, *override.ConfidenceThreshold, ErrInvalidConfig)
			}
		}
		if override.MaxMappingsPerControl != nil {
			if *override.MaxMappingsPerControl <= 0 {
				return fmt.Errorf("synthesis.tenant_overrides.%s.max_mappings_per_control %d must be positive: %w",
					key, *override.MaxMappingsPerControl, ErrInvalidConfig)
			}
		}
		if override.Viability != nil {
			v := override.Viability
			if v.TypeMismatchFactor <= 0 || v.TypeMismatchFactor > 2 {
				return fmt.Errorf("synthesis.tenant_overrides.%s.viability.type_mismatch_factor %g must be in range (0, 2]: %w",
					key, v.TypeMismatchFactor, ErrInvalidConfig)
			}
			if v.SkipLevelFactor <= 0 || v.SkipLevelFactor > 2 {
				return fmt.Errorf("synthesis.tenant_overrides.%s.viability.skip_level_factor %g must be in range (0, 2]: %w",
					key, v.SkipLevelFactor, ErrInvalidConfig)
			}
			if v.IntegralToFactor <= 0 || v.IntegralToFactor > 2 {
				return fmt.Errorf("synthesis.tenant_overrides.%s.viability.integral_to_factor %g must be in range (0, 2]: %w",
					key, v.IntegralToFactor, ErrInvalidConfig)
			}
		}
		if override.Assessment != nil {
			a := override.Assessment
			if a.IQRGood <= 0 {
				return fmt.Errorf("synthesis.tenant_overrides.%s.assessment.iqr_good %g must be positive: %w",
					key, a.IQRGood, ErrInvalidConfig)
			}
			if a.IQRPoor <= 0 {
				return fmt.Errorf("synthesis.tenant_overrides.%s.assessment.iqr_poor %g must be positive: %w",
					key, a.IQRPoor, ErrInvalidConfig)
			}
			if a.IQRGood <= a.IQRPoor {
				return fmt.Errorf("synthesis.tenant_overrides.%s.assessment.iqr_good %g must be greater than iqr_poor %g: %w",
					key, a.IQRGood, a.IQRPoor, ErrInvalidConfig)
			}
			if a.NoRelHigh < 0 || a.NoRelHigh > 1 {
				return fmt.Errorf("synthesis.tenant_overrides.%s.assessment.no_rel_high %g must be in range [0, 1]: %w",
					key, a.NoRelHigh, ErrInvalidConfig)
			}
			if a.NoRelLow < 0 || a.NoRelLow > 1 {
				return fmt.Errorf("synthesis.tenant_overrides.%s.assessment.no_rel_low %g must be in range [0, 1]: %w",
					key, a.NoRelLow, ErrInvalidConfig)
			}
			if a.NoRelHigh <= a.NoRelLow {
				return fmt.Errorf("synthesis.tenant_overrides.%s.assessment.no_rel_high %g must be greater than no_rel_low %g: %w",
					key, a.NoRelHigh, a.NoRelLow, ErrInvalidConfig)
			}
			if a.ContestedWarn < 0 || a.ContestedWarn > 1 {
				return fmt.Errorf("synthesis.tenant_overrides.%s.assessment.contested_warn %g must be in range [0, 1]: %w",
					key, a.ContestedWarn, ErrInvalidConfig)
			}
			if a.ActionableWarn < 0 || a.ActionableWarn > 1 {
				return fmt.Errorf("synthesis.tenant_overrides.%s.assessment.actionable_warn %g must be in range [0, 1]: %w",
					key, a.ActionableWarn, ErrInvalidConfig)
			}
		}
	}

	return nil
}
