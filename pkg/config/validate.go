package config

import (
	"fmt"
	"regexp"
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

	// Validate requires config
	if err := a.Requires.Validate(); err != nil {
		return err
	}

	return nil
}
