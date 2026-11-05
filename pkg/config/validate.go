package config

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

const (
	// maxLookupBatchSize bounds analysis.result_cache.lookup_batch_size: one
	// lookup binds that many keys in a single ANY($3) array.
	maxLookupBatchSize = 10000
	// maxModelRevisionBytes bounds an llm.model_revisions value, which is
	// part of every cache key and shown in run status.
	maxModelRevisionBytes = 128
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

// validate checks every section of cfg and returns all their errors joined
// (nil if none), so one load reports every problem. Each is a FieldError
// naming the setting it concerns.
func validate(cfg *Config, tracker *sourceTracker) error {
	err := errors.Join(
		validateTLS(&cfg.TLS, tracker),
		validateStorage(&cfg.Storage, tracker),
		validateLogging(&cfg.Logging, tracker),
		validateLLM(&cfg.LLM, tracker),
		validateCatalog(&cfg.Catalog, tracker),
		validateAttestation(&cfg.Attestation, tracker),
		validatePrompt(&cfg.Prompt, tracker),
		validateAnalysis(&cfg.Analysis, tracker),
		cfg.Worker.validate(tracker),
		cfg.Pipeline.validate(tracker),
		validateSynthesis(&cfg.Synthesis, tracker),
		cfg.Retention.validate(tracker),
		validateBackup(&cfg.Backup, &cfg.Storage.Objects, tracker),
		validateGraph(&cfg.Graph, tracker),
		validateTenants(&cfg.Tenants, tracker),
		validateRole(cfg, tracker),
	)
	if tracker != nil {
		for _, fe := range FieldErrors(err) {
			if fe.Source == "" {
				fe.Source = tracker.sourceOfSetting(fe.Path)
			}
		}
	}
	return err
}

// problems collects one validator's FieldErrors.
type problems []error

func (p *problems) add(path, format string, args ...any) {
	*p = append(*p, fieldErrorf(path, format, args...))
}

// addFrom adds a problem whose Source is source, the layer that set the
// value which makes path a problem (such as the backend that requires a
// missing setting), rather than whatever set path itself.
func (p *problems) addFrom(source, path, format string, args ...any) {
	*p = append(*p, &FieldError{Path: path, Source: source, Err: fmt.Errorf(format, args...)})
}

func (p problems) err() error { return errors.Join(p...) }

func validateTLS(tls *TLSConfig, tracker *sourceTracker) error {
	var p problems
	if tls.Mode != "" && !validTLSModes[tls.Mode] {
		p.add("tls.mode", "tls.mode %q%s must be one of off, server-only, mutual: %w",
			tls.Mode, formatSource(tracker, "tls.mode"), ErrInvalidConfig)
	}

	// A mode's requirements are reported under tls.mode, the setting that
	// makes them required: it stays one problem until all are met.
	switch tls.Mode {
	case "mutual":
		if tls.Cert == "" || tls.Key == "" || tls.CA == "" {
			p.add("tls.mode", "tls.mode mutual%s requires cert, key, and ca: %w",
				formatSource(tracker, "tls.mode"), ErrInvalidConfig)
		}
	case "server-only":
		if tls.Cert == "" || tls.Key == "" {
			p.add("tls.mode", "tls.mode server-only%s requires cert and key: %w",
				formatSource(tracker, "tls.mode"), ErrInvalidConfig)
		}
	}

	// Validate per-target overrides
	for _, name := range slices.Sorted(maps.Keys(tls.Targets)) {
		override := tls.Targets[name]
		if override.Mode != "" && !validTLSModes[override.Mode] {
			p.add(mapKeyPath("tls.targets", name)+".mode", "tls.targets.%s.mode %q must be one of off, server-only, mutual: %w",
				name, override.Mode, ErrInvalidConfig)
		}
	}

	return p.err()
}

func validateStorage(s *StorageConfig, tracker *sourceTracker) error {
	var p problems
	if s.Objects.Backend != "" && !validStorageBackends[s.Objects.Backend] {
		p.add("storage.objects.backend", "storage.objects.backend %q%s must be one of local, s3: %w",
			s.Objects.Backend, formatSource(tracker, "storage.objects.backend"), ErrInvalidConfig)
	}
	return p.err()
}

func validateLogging(l *LoggingConfig, tracker *sourceTracker) error {
	var p problems
	if l.Level != "" && !validLogLevels[l.Level] {
		p.add("logging.level", "logging.level %q%s must be one of debug, info, warn, error: %w",
			l.Level, formatSource(tracker, "logging.level"), ErrInvalidConfig)
	}
	if l.Format != "" && !validLogFormats[l.Format] {
		p.add("logging.format", "logging.format %q%s must be one of text, json: %w",
			l.Format, formatSource(tracker, "logging.format"), ErrInvalidConfig)
	}
	return p.err()
}

func validateLLM(llm *LLMConfig, tracker *sourceTracker) error {
	var p problems
	if llm.Timeout < 0 {
		p.add("llm.timeout", "llm.timeout %d%s must be non-negative: %w",
			llm.Timeout, formatSource(tracker, "llm.timeout"), ErrInvalidConfig)
	}
	if llm.MaxRetries < 0 {
		p.add("llm.max_retries", "llm.max_retries %d%s must be non-negative: %w",
			llm.MaxRetries, formatSource(tracker, "llm.max_retries"), ErrInvalidConfig)
	}
	if llm.GatewayMode && llm.GatewayURL == "" {
		p.add("llm.gateway_url", "llm.gateway_url%s must be set when llm.gateway_mode is true; "+
			"set gateway_url to your LLM proxy address (e.g. http://litellm:4000): %w",
			formatSource(tracker, "llm.gateway_url"), ErrInvalidConfig)
	}
	// When a gateway handles retries, client-side retries are redundant.
	// Normalize to zero so llmclient does not need its own check.
	if llm.GatewayMode && llm.MaxRetries > 0 {
		llm.MaxRetries = 0
	}
	for _, key := range slices.Sorted(maps.Keys(llm.TenantOverrides)) {
		override := llm.TenantOverrides[key]
		if err := tenant.ValidateTenantID(key); err != nil {
			p.add(mapKeyPath("llm.tenant_overrides", key), "llm.tenant_overrides key %q is not a valid tenant ID: %w",
				key, ErrInvalidConfig)
			continue
		}
		if override.MaxRetries != nil && *override.MaxRetries < 0 {
			p.add(mapKeyPath("llm.tenant_overrides", key)+".max_retries", "llm.tenant_overrides.%s.max_retries %d must be non-negative: %w",
				key, *override.MaxRetries, ErrInvalidConfig)
		}
		if override.Timeout != nil && *override.Timeout < 0 {
			p.add(mapKeyPath("llm.tenant_overrides", key)+".timeout", "llm.tenant_overrides.%s.timeout %d must be non-negative: %w",
				key, *override.Timeout, ErrInvalidConfig)
		}
	}
	for _, alias := range slices.Sorted(maps.Keys(llm.ModelRevisions)) {
		rev := llm.ModelRevisions[alias]
		path := mapKeyPath("llm.model_revisions", alias)
		switch {
		case strings.TrimSpace(alias) == "":
			p.add(path, "llm.model_revisions has an empty alias%s: each key must be a model alias the gateway serves, such as \"default\": %w",
				formatSource(tracker, "llm.model_revisions"), ErrInvalidConfig)
		case rev == "":
			p.add(path, "llm.model_revisions.%s is empty%s: an empty revision means no revision, so omit the alias instead: %w",
				alias, formatSource(tracker, path), ErrInvalidConfig)
		case len(rev) > maxModelRevisionBytes:
			p.add(path, "llm.model_revisions.%s is %d bytes%s, over the %d-byte limit: use a short label such as a date or a digest prefix: %w",
				alias, len(rev), formatSource(tracker, path), maxModelRevisionBytes, ErrInvalidConfig)
		case !utf8.ValidString(rev) || strings.IndexFunc(rev, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0:
			p.add(path, "llm.model_revisions.%s %q%s contains a non-printable character: use letters, digits and punctuation: %w",
				alias, rev, formatSource(tracker, path), ErrInvalidConfig)
		}
	}
	return p.err()
}

func validateAttestation(a *AttestationConfig, tracker *sourceTracker) error {
	var p problems
	if a.ExpiryDuration <= 0 {
		p.add("attestation.expiry_duration", "attestation.expiry_duration %s%s must be positive: %w",
			a.ExpiryDuration, formatSource(tracker, "attestation.expiry_duration"), ErrInvalidConfig)
	}
	if a.StorageRetries < 0 || a.StorageRetries > 10 {
		p.add("attestation.storage_retries", "attestation.storage_retries %d%s must be in range [0, 10]: %w",
			a.StorageRetries, formatSource(tracker, "attestation.storage_retries"), ErrInvalidConfig)
	}
	if a.StorageRetryDelay < 0 || a.StorageRetryDelay > time.Minute {
		p.add("attestation.storage_retry_delay", "attestation.storage_retry_delay %s%s must be in range [0, 1m]: %w",
			a.StorageRetryDelay, formatSource(tracker, "attestation.storage_retry_delay"), ErrInvalidConfig)
	}
	// A key pair with one half missing is reported under the half that is
	// set, the setting the operator added.
	if (a.PrivateKeyPath == "") != (a.PublicKeyPath == "") {
		path := "attestation.private_key_path"
		if a.PrivateKeyPath == "" {
			path = "attestation.public_key_path"
		}
		p.add(path, "attestation.private_key_path and attestation.public_key_path%s must both be set or both empty: %w",
			formatSource(tracker, path), ErrInvalidConfig)
	}
	for _, tenantID := range slices.Sorted(maps.Keys(a.TenantOverrides)) {
		override := a.TenantOverrides[tenantID]
		if err := tenant.ValidateTenantID(tenantID); err != nil {
			p.add(mapKeyPath("attestation.tenant_overrides", tenantID), "attestation.tenant_overrides: invalid tenant ID %q: %w", tenantID, ErrInvalidConfig)
			continue
		}
		if override.ExpiryDuration != nil && *override.ExpiryDuration <= 0 {
			p.add(mapKeyPath("attestation.tenant_overrides", tenantID)+".expiry_duration", "attestation.tenant_overrides.%s.expiry_duration %s must be positive: %w",
				tenantID, *override.ExpiryDuration, ErrInvalidConfig)
		}
		privSet := override.PrivateKeyPath != nil && *override.PrivateKeyPath != ""
		pubSet := override.PublicKeyPath != nil && *override.PublicKeyPath != ""
		if privSet != pubSet {
			half := "public_key_path"
			if privSet {
				half = "private_key_path"
			}
			p.add(mapKeyPath("attestation.tenant_overrides", tenantID)+"."+half, "attestation.tenant_overrides.%s.private_key_path and public_key_path must both be set or both empty: %w",
				tenantID, ErrInvalidConfig)
		}
	}
	return p.err()
}

func validateCatalog(cat *CatalogConfig, tracker *sourceTracker) error {
	var p problems
	s := &cat.Structuring
	if s.MinDecomposeWords < 0 {
		p.add("catalog.structuring.min_decompose_words", "catalog.structuring.min_decompose_words %d%s must be non-negative: %w",
			s.MinDecomposeWords, formatSource(tracker, "catalog.structuring.min_decompose_words"), ErrInvalidConfig)
	}
	if s.ChunkChars < 0 {
		p.add("catalog.structuring.chunk_chars", "catalog.structuring.chunk_chars %d%s must be non-negative: %w",
			s.ChunkChars, formatSource(tracker, "catalog.structuring.chunk_chars"), ErrInvalidConfig)
	}
	if s.MaxValidationChars < 0 {
		p.add("catalog.structuring.max_validation_chars", "catalog.structuring.max_validation_chars %d%s must be non-negative: %w",
			s.MaxValidationChars, formatSource(tracker, "catalog.structuring.max_validation_chars"), ErrInvalidConfig)
	}
	if s.MaxHeadingRepeats < 0 {
		p.add("catalog.structuring.max_heading_repeats", "catalog.structuring.max_heading_repeats %d%s must be non-negative: %w",
			s.MaxHeadingRepeats, formatSource(tracker, "catalog.structuring.max_heading_repeats"), ErrInvalidConfig)
	}
	if s.SectionPattern != "" {
		if _, err := regexp.Compile(s.SectionPattern); err != nil {
			p.add("catalog.structuring.section_pattern", "catalog.structuring.section_pattern %q%s is not valid regex: %w",
				s.SectionPattern, formatSource(tracker, "catalog.structuring.section_pattern"), ErrInvalidConfig)
		}
	}
	return p.err()
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
	var p problems
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
				p.add(fmt.Sprintf("prompt.layers.order[%d].id", i), "prompt.layers.order[%d].id %q%s must be one of embedded, user, project, cli or a path from layer_paths: %w",
					i, entry.ID, formatSource(tracker, "prompt.layers.order"), ErrInvalidConfig)
			}
		}

		// Check for duplicates
		if seen[entry.ID] && (validLayerIDs[entry.ID] || slices.Contains(prompt.LayerPaths, entry.ID)) {
			p.add(fmt.Sprintf("prompt.layers.order[%d].id", i), "prompt.layers.order[%d].id %q%s is a duplicate layer ID: %w",
				i, entry.ID, formatSource(tracker, "prompt.layers.order"), ErrInvalidConfig)
		}
		seen[entry.ID] = true

		// Validate merge mode
		if !validMergeModes[entry.Merge] {
			p.add(fmt.Sprintf("prompt.layers.order[%d].merge", i), "prompt.layers.order[%d].merge %q%s must be one of merge, replace: %w",
				i, entry.Merge, formatSource(tracker, "prompt.layers.order"), ErrInvalidConfig)
		}

		// Validate slice strategy
		if !validSliceStrategies[entry.SliceStrategy] {
			p.add(fmt.Sprintf("prompt.layers.order[%d].slice_strategy", i), "prompt.layers.order[%d].slice_strategy %q%s must be one of replace, append, deep_copy: %w",
				i, entry.SliceStrategy, formatSource(tracker, "prompt.layers.order"), ErrInvalidConfig)
		}
	}

	for _, key := range slices.Sorted(maps.Keys(prompt.TenantOverrides)) {
		if err := tenant.ValidateTenantID(key); err != nil {
			p.add(mapKeyPath("prompt.tenant_overrides", key), "prompt.tenant_overrides key %q is not a valid tenant ID: it must be %s: %w",
				key, tenant.IDRule, ErrInvalidConfig)
		}
	}

	return p.err()
}

func validateAnalysis(a *AnalysisConfig, tracker *sourceTracker) error {
	var p problems
	c := &a.Classification
	if c.MaxTextLength <= 0 {
		p.add("analysis.classification.max_text_length", "analysis.classification.max_text_length %d%s must be positive: %w",
			c.MaxTextLength, formatSource(tracker, "analysis.classification.max_text_length"), ErrInvalidConfig)
	}
	if c.MaxTokens <= 0 {
		p.add("analysis.classification.max_tokens", "analysis.classification.max_tokens %d%s must be positive: %w",
			c.MaxTokens, formatSource(tracker, "analysis.classification.max_tokens"), ErrInvalidConfig)
	}
	if math.IsNaN(c.Temperature) || c.Temperature < 0 {
		p.add("analysis.classification.temperature", "analysis.classification.temperature %g%s must be non-negative: %w",
			c.Temperature, formatSource(tracker, "analysis.classification.temperature"), ErrInvalidConfig)
	} else if c.Temperature > 2.0 {
		p.add("analysis.classification.temperature", "analysis.classification.temperature %g%s must not exceed 2.0: %w",
			c.Temperature, formatSource(tracker, "analysis.classification.temperature"), ErrInvalidConfig)
	}
	e := &a.Embedding
	if e.MaxChars < 0 {
		p.add("analysis.embedding.max_chars", "analysis.embedding.max_chars %d%s must be non-negative: %w",
			e.MaxChars, formatSource(tracker, "analysis.embedding.max_chars"), ErrInvalidConfig)
	}
	if e.BatchSize <= 0 {
		p.add("analysis.embedding.batch_size", "analysis.embedding.batch_size %d%s must be positive: %w",
			e.BatchSize, formatSource(tracker, "analysis.embedding.batch_size"), ErrInvalidConfig)
	}
	if e.Enabled && len(e.Models) == 0 {
		p.add("analysis.embedding.models", "analysis.embedding.models%s must not be empty when enabled: %w",
			formatSource(tracker, "analysis.embedding.models"), ErrInvalidConfig)
	}
	r := &a.Relationship
	if r.MaxSourceChars <= 0 {
		p.add("analysis.relationship.max_source_chars", "analysis.relationship.max_source_chars %d%s must be positive: %w",
			r.MaxSourceChars, formatSource(tracker, "analysis.relationship.max_source_chars"), ErrInvalidConfig)
	}
	if r.MaxTargetChars <= 0 {
		p.add("analysis.relationship.max_target_chars", "analysis.relationship.max_target_chars %d%s must be positive: %w",
			r.MaxTargetChars, formatSource(tracker, "analysis.relationship.max_target_chars"), ErrInvalidConfig)
	}
	if r.MaxTokens <= 0 {
		p.add("analysis.relationship.max_tokens", "analysis.relationship.max_tokens %d%s must be positive: %w",
			r.MaxTokens, formatSource(tracker, "analysis.relationship.max_tokens"), ErrInvalidConfig)
	}
	if r.SamplesPerModel <= 0 {
		p.add("analysis.relationship.samples_per_model", "analysis.relationship.samples_per_model %d%s must be positive: %w",
			r.SamplesPerModel, formatSource(tracker, "analysis.relationship.samples_per_model"), ErrInvalidConfig)
	}
	if math.IsNaN(r.SamplingTemperature) || r.SamplingTemperature < 0 {
		p.add("analysis.relationship.sampling_temperature", "analysis.relationship.sampling_temperature %g%s must be non-negative: %w",
			r.SamplingTemperature, formatSource(tracker, "analysis.relationship.sampling_temperature"), ErrInvalidConfig)
	} else if r.SamplingTemperature > 2.0 {
		p.add("analysis.relationship.sampling_temperature", "analysis.relationship.sampling_temperature %g%s must not exceed 2.0: %w",
			r.SamplingTemperature, formatSource(tracker, "analysis.relationship.sampling_temperature"), ErrInvalidConfig)
	}
	if r.Enabled && len(r.Models) == 0 {
		p.add("analysis.relationship.models", "analysis.relationship.models%s must not be empty when enabled: %w",
			formatSource(tracker, "analysis.relationship.models"), ErrInvalidConfig)
	}
	p = append(p, validateConfidenceWeights(&r.ConfidenceWeights, tracker))
	for _, at := range r.ActionableTypes {
		if !validRelationshipTypes[at] {
			p.add("analysis.relationship.actionable_types", "analysis.relationship.actionable_types: invalid type %q%s: %w",
				at, formatSource(tracker, "analysis.relationship.actionable_types"), ErrInvalidConfig)
		}
	}

	p = append(p,
		a.Engine.validate(tracker),
		a.Requires.validate(tracker),
		a.Artifacts.validate(tracker),
		a.ArtifactAdjudication.validate(tracker),
		a.Candidates.validate(tracker),
		validateCandidateGenerators(a.Candidates.Generators, tracker),
	)

	p = append(p, validateAnalyzerDependencies(a, tracker))

	// An empty embed_model or models list is already reported above.
	if e.Enabled && a.Candidates.EmbedModel != "" && len(e.Models) > 0 {
		found := false
		for _, m := range e.Models {
			if m == a.Candidates.EmbedModel {
				found = true
				break
			}
		}
		if !found {
			p.add("analysis.candidates.embed_model", "analysis.candidates.embed_model %q%s must be one of analysis.embedding.models %v: %w",
				a.Candidates.EmbedModel, formatSource(tracker, "analysis.candidates.embed_model"), e.Models, ErrInvalidConfig)
		}
	}

	rc := &a.ResultCache
	if rc.LookupBatchSize < 1 || rc.LookupBatchSize > maxLookupBatchSize {
		p.add("analysis.result_cache.lookup_batch_size", "analysis.result_cache.lookup_batch_size %d%s must be between 1 and %d: "+
			"it is how many cache keys one lookup query reads (default 1000): %w",
			rc.LookupBatchSize, formatSource(tracker, "analysis.result_cache.lookup_batch_size"), maxLookupBatchSize, ErrInvalidConfig)
	}
	for _, key := range slices.Sorted(maps.Keys(rc.TenantOverrides)) {
		path := mapKeyPath("analysis.result_cache.tenant_overrides", key)
		if err := tenant.ValidateTenantID(key); err != nil {
			p.add(path, "analysis.result_cache.tenant_overrides key %q is not a valid tenant ID: it must be %s: %w",
				key, tenant.IDRule, ErrInvalidConfig)
			continue
		}
		if n := rc.TenantOverrides[key].LookupBatchSize; n != nil && (*n < 1 || *n > maxLookupBatchSize) {
			p.add(path+".lookup_batch_size", "analysis.result_cache.tenant_overrides.%s.lookup_batch_size %d%s must be between 1 and %d: "+
				"it is how many cache keys one lookup query reads (default 1000): %w",
				key, *n, formatSource(tracker, path+".lookup_batch_size"), maxLookupBatchSize, ErrInvalidConfig)
		}
	}

	return p.err()
}

func validateGraph(g *GraphConfig, tracker *sourceTracker) error {
	if g.MaxBulkEdges < 1 || g.MaxBulkEdges > maxGraphMaxBulkEdges {
		return fieldErrorf("graph.max_bulk_edges", "graph.max_bulk_edges %d%s must be in range [1, %d]; it bounds the edges in one BulkCreateEdges request (default %d): %w",
			g.MaxBulkEdges, formatSource(tracker, "graph.max_bulk_edges"), maxGraphMaxBulkEdges, DefaultGraphMaxBulkEdges, ErrInvalidConfig)
	}
	return nil
}

// validateTenants rejects a malformed default_tenant at load time, before
// bootstrap uses it as a database key and AGE graph name. Empty is valid
// here: roles that need a default tenant reject it at startup.
func validateTenants(t *TenantsConfig, tracker *sourceTracker) error {
	if t.DefaultTenant == "" {
		return nil
	}
	if err := tenant.ValidateTenantID(t.DefaultTenant); err != nil {
		return fieldErrorf("tenants.default_tenant", "tenants.default_tenant %q%s is not a valid tenant ID: it must be %s. Set it to an ID that matches, or leave it empty for roles that do not provision a default tenant: %w",
			t.DefaultTenant, formatSource(tracker, "tenants.default_tenant"), tenant.IDRule, ErrInvalidConfig)
	}
	return nil
}

func validateRole(cfg *Config, tracker *sourceTracker) error {
	if cfg.Role == "" {
		return nil
	}
	canonical, err := ResolveRole(cfg.Role)
	if err != nil {
		// %w (not %s) preserves the wrapped ErrInvalidConfig so
		// errors.Is(result, ErrInvalidConfig) still matches -- every other
		// validator in this file relies on that same chain.
		return fieldErrorf("role", "%w%s", err, formatSource(tracker, "role"))
	}
	cfg.Role = canonical
	return nil
}

func validateSynthesis(s *SynthesisConfig, tracker *sourceTracker) error {
	var p problems
	// Viability factors: must be > 0 and <= 2
	if math.IsNaN(s.Viability.TypeMismatchFactor) || s.Viability.TypeMismatchFactor <= 0 || s.Viability.TypeMismatchFactor > 2 {
		p.add("synthesis.viability.type_mismatch_factor", "synthesis.viability.type_mismatch_factor %g%s must be in range (0, 2]: %w",
			s.Viability.TypeMismatchFactor, formatSource(tracker, "synthesis.viability.type_mismatch_factor"), ErrInvalidConfig)
	}
	if math.IsNaN(s.Viability.SkipLevelFactor) || s.Viability.SkipLevelFactor <= 0 || s.Viability.SkipLevelFactor > 2 {
		p.add("synthesis.viability.skip_level_factor", "synthesis.viability.skip_level_factor %g%s must be in range (0, 2]: %w",
			s.Viability.SkipLevelFactor, formatSource(tracker, "synthesis.viability.skip_level_factor"), ErrInvalidConfig)
	}
	if math.IsNaN(s.Viability.IntegralToFactor) || s.Viability.IntegralToFactor <= 0 || s.Viability.IntegralToFactor > 2 {
		p.add("synthesis.viability.integral_to_factor", "synthesis.viability.integral_to_factor %g%s must be in range (0, 2]: %w",
			s.Viability.IntegralToFactor, formatSource(tracker, "synthesis.viability.integral_to_factor"), ErrInvalidConfig)
	}

	// Assessment: IQRGood > IQRPoor, both > 0
	if math.IsNaN(s.Assessment.IQRGood) || s.Assessment.IQRGood <= 0 {
		p.add("synthesis.assessment.iqr_good", "synthesis.assessment.iqr_good %g%s must be positive: %w",
			s.Assessment.IQRGood, formatSource(tracker, "synthesis.assessment.iqr_good"), ErrInvalidConfig)
	}
	if math.IsNaN(s.Assessment.IQRPoor) || s.Assessment.IQRPoor <= 0 {
		p.add("synthesis.assessment.iqr_poor", "synthesis.assessment.iqr_poor %g%s must be positive: %w",
			s.Assessment.IQRPoor, formatSource(tracker, "synthesis.assessment.iqr_poor"), ErrInvalidConfig)
	}
	// The comparisons below are checked only between values that are each
	// valid, so one bad value is one problem.
	if s.Assessment.IQRGood > 0 && s.Assessment.IQRPoor > 0 && s.Assessment.IQRGood <= s.Assessment.IQRPoor {
		p.add("synthesis.assessment.iqr_good", "synthesis.assessment.iqr_good %g%s must be greater than iqr_poor %g: %w",
			s.Assessment.IQRGood, formatSource(tracker, "synthesis.assessment.iqr_good"), s.Assessment.IQRPoor, ErrInvalidConfig)
	}

	// Fraction thresholds: must be in [0, 1]
	if math.IsNaN(s.Assessment.NoRelHigh) || s.Assessment.NoRelHigh < 0 || s.Assessment.NoRelHigh > 1 {
		p.add("synthesis.assessment.no_rel_high", "synthesis.assessment.no_rel_high %g%s must be in range [0, 1]: %w",
			s.Assessment.NoRelHigh, formatSource(tracker, "synthesis.assessment.no_rel_high"), ErrInvalidConfig)
	}
	if math.IsNaN(s.Assessment.NoRelLow) || s.Assessment.NoRelLow < 0 || s.Assessment.NoRelLow > 1 {
		p.add("synthesis.assessment.no_rel_low", "synthesis.assessment.no_rel_low %g%s must be in range [0, 1]: %w",
			s.Assessment.NoRelLow, formatSource(tracker, "synthesis.assessment.no_rel_low"), ErrInvalidConfig)
	}
	if inUnitRange(s.Assessment.NoRelHigh) && inUnitRange(s.Assessment.NoRelLow) && s.Assessment.NoRelHigh <= s.Assessment.NoRelLow {
		p.add("synthesis.assessment.no_rel_high", "synthesis.assessment.no_rel_high %g%s must be greater than no_rel_low %g: %w",
			s.Assessment.NoRelHigh, formatSource(tracker, "synthesis.assessment.no_rel_high"), s.Assessment.NoRelLow, ErrInvalidConfig)
	}
	if math.IsNaN(s.Assessment.ContestedWarn) || s.Assessment.ContestedWarn < 0 || s.Assessment.ContestedWarn > 1 {
		p.add("synthesis.assessment.contested_warn", "synthesis.assessment.contested_warn %g%s must be in range [0, 1]: %w",
			s.Assessment.ContestedWarn, formatSource(tracker, "synthesis.assessment.contested_warn"), ErrInvalidConfig)
	}
	if math.IsNaN(s.Assessment.ActionableWarn) || s.Assessment.ActionableWarn < 0 || s.Assessment.ActionableWarn > 1 {
		p.add("synthesis.assessment.actionable_warn", "synthesis.assessment.actionable_warn %g%s must be in range [0, 1]: %w",
			s.Assessment.ActionableWarn, formatSource(tracker, "synthesis.assessment.actionable_warn"), ErrInvalidConfig)
	}

	// Global confidence threshold and max mappings
	if math.IsNaN(s.ConfidenceThreshold) || s.ConfidenceThreshold < 0 || s.ConfidenceThreshold > 1 {
		p.add("synthesis.confidence_threshold", "synthesis.confidence_threshold %g%s must be in range [0, 1]: %w",
			s.ConfidenceThreshold, formatSource(tracker, "synthesis.confidence_threshold"), ErrInvalidConfig)
	}
	if s.MaxMappingsPerControl <= 0 {
		p.add("synthesis.max_mappings_per_control", "synthesis.max_mappings_per_control %d%s must be positive: %w",
			s.MaxMappingsPerControl, formatSource(tracker, "synthesis.max_mappings_per_control"), ErrInvalidConfig)
	}

	// Validate tenant overrides
	for _, key := range slices.Sorted(maps.Keys(s.TenantOverrides)) {
		override := s.TenantOverrides[key]
		if err := tenant.ValidateTenantID(key); err != nil {
			p.add(mapKeyPath("synthesis.tenant_overrides", key), "synthesis.tenant_overrides key %q is not a valid tenant ID: %w",
				key, ErrInvalidConfig)
			continue
		}
		if override.ConfidenceThreshold != nil {
			if math.IsNaN(*override.ConfidenceThreshold) || *override.ConfidenceThreshold < 0 || *override.ConfidenceThreshold > 1 {
				p.add(mapKeyPath("synthesis.tenant_overrides", key)+".confidence_threshold", "synthesis.tenant_overrides.%s.confidence_threshold %g must be in range [0, 1]: %w",
					key, *override.ConfidenceThreshold, ErrInvalidConfig)
			}
		}
		if override.MaxMappingsPerControl != nil {
			if *override.MaxMappingsPerControl <= 0 {
				p.add(mapKeyPath("synthesis.tenant_overrides", key)+".max_mappings_per_control", "synthesis.tenant_overrides.%s.max_mappings_per_control %d must be positive: %w",
					key, *override.MaxMappingsPerControl, ErrInvalidConfig)
			}
		}
		if override.Viability != nil {
			v := override.Viability
			if math.IsNaN(v.TypeMismatchFactor) || v.TypeMismatchFactor <= 0 || v.TypeMismatchFactor > 2 {
				p.add(mapKeyPath("synthesis.tenant_overrides", key)+".viability.type_mismatch_factor", "synthesis.tenant_overrides.%s.viability.type_mismatch_factor %g must be in range (0, 2]: %w",
					key, v.TypeMismatchFactor, ErrInvalidConfig)
			}
			if math.IsNaN(v.SkipLevelFactor) || v.SkipLevelFactor <= 0 || v.SkipLevelFactor > 2 {
				p.add(mapKeyPath("synthesis.tenant_overrides", key)+".viability.skip_level_factor", "synthesis.tenant_overrides.%s.viability.skip_level_factor %g must be in range (0, 2]: %w",
					key, v.SkipLevelFactor, ErrInvalidConfig)
			}
			if math.IsNaN(v.IntegralToFactor) || v.IntegralToFactor <= 0 || v.IntegralToFactor > 2 {
				p.add(mapKeyPath("synthesis.tenant_overrides", key)+".viability.integral_to_factor", "synthesis.tenant_overrides.%s.viability.integral_to_factor %g must be in range (0, 2]: %w",
					key, v.IntegralToFactor, ErrInvalidConfig)
			}
		}
		if override.Assessment != nil {
			a := override.Assessment
			if math.IsNaN(a.IQRGood) || a.IQRGood <= 0 {
				p.add(mapKeyPath("synthesis.tenant_overrides", key)+".assessment.iqr_good", "synthesis.tenant_overrides.%s.assessment.iqr_good %g must be positive: %w",
					key, a.IQRGood, ErrInvalidConfig)
			}
			if math.IsNaN(a.IQRPoor) || a.IQRPoor <= 0 {
				p.add(mapKeyPath("synthesis.tenant_overrides", key)+".assessment.iqr_poor", "synthesis.tenant_overrides.%s.assessment.iqr_poor %g must be positive: %w",
					key, a.IQRPoor, ErrInvalidConfig)
			}
			if a.IQRGood > 0 && a.IQRPoor > 0 && a.IQRGood <= a.IQRPoor {
				p.add(mapKeyPath("synthesis.tenant_overrides", key)+".assessment.iqr_good", "synthesis.tenant_overrides.%s.assessment.iqr_good %g must be greater than iqr_poor %g: %w",
					key, a.IQRGood, a.IQRPoor, ErrInvalidConfig)
			}
			if math.IsNaN(a.NoRelHigh) || a.NoRelHigh < 0 || a.NoRelHigh > 1 {
				p.add(mapKeyPath("synthesis.tenant_overrides", key)+".assessment.no_rel_high", "synthesis.tenant_overrides.%s.assessment.no_rel_high %g must be in range [0, 1]: %w",
					key, a.NoRelHigh, ErrInvalidConfig)
			}
			if math.IsNaN(a.NoRelLow) || a.NoRelLow < 0 || a.NoRelLow > 1 {
				p.add(mapKeyPath("synthesis.tenant_overrides", key)+".assessment.no_rel_low", "synthesis.tenant_overrides.%s.assessment.no_rel_low %g must be in range [0, 1]: %w",
					key, a.NoRelLow, ErrInvalidConfig)
			}
			if inUnitRange(a.NoRelHigh) && inUnitRange(a.NoRelLow) && a.NoRelHigh <= a.NoRelLow {
				p.add(mapKeyPath("synthesis.tenant_overrides", key)+".assessment.no_rel_high", "synthesis.tenant_overrides.%s.assessment.no_rel_high %g must be greater than no_rel_low %g: %w",
					key, a.NoRelHigh, a.NoRelLow, ErrInvalidConfig)
			}
			if math.IsNaN(a.ContestedWarn) || a.ContestedWarn < 0 || a.ContestedWarn > 1 {
				p.add(mapKeyPath("synthesis.tenant_overrides", key)+".assessment.contested_warn", "synthesis.tenant_overrides.%s.assessment.contested_warn %g must be in range [0, 1]: %w",
					key, a.ContestedWarn, ErrInvalidConfig)
			}
			if math.IsNaN(a.ActionableWarn) || a.ActionableWarn < 0 || a.ActionableWarn > 1 {
				p.add(mapKeyPath("synthesis.tenant_overrides", key)+".assessment.actionable_warn", "synthesis.tenant_overrides.%s.assessment.actionable_warn %g must be in range [0, 1]: %w",
					key, a.ActionableWarn, ErrInvalidConfig)
			}
		}
	}

	return p.err()
}

// inUnitRange reports whether f is in [0, 1].
func inUnitRange(f float64) bool { return f >= 0 && f <= 1 }

// generatorSetting is one key a builtin candidate generator reads from its
// GenerateRequest.Parameters, with the rule its value must satisfy.
type generatorSetting struct {
	valid func(v interface{}) bool
	rule  string
}

// candidateGeneratorSettings lists the config keys each builtin generator in
// pkg/analyzer/candidate/builtin reads (see each Generate doc comment). An
// unlisted key would be ignored by the generator, so it is rejected here.
// "weight" is not a config key: it is the entry's own Weight field.
var candidateGeneratorSettings = map[string]map[string]generatorSetting{
	"keyword": {
		"keywords": {valid: isNonEmptyStringList, rule: "must be a non-empty list of non-empty strings"},
	},
	"level": {},
	"semantic": {
		"top_k":          {valid: isPositiveInteger, rule: "must be a positive integer"},
		"min_similarity": {valid: isSimilarityThreshold, rule: "must be a number in range [0, 100] (cosine similarity x 100)"},
	},
}

// maxExactFloatInt is the largest integer float64 represents exactly; a
// JSON-decoded top_k above it cannot be converted to int faithfully.
//
// The accepted number shapes here (int, int64, integral float64) mirror the
// generators' readers in pkg/analyzer/candidate/builtin/parameters.go, which
// pkg/config must not import; change both together.
const maxExactFloatInt = 1 << 53

// isPositiveInteger accepts the integer shapes a decoder produces: YAML
// yields int, JSON yields float64, so an integral float64 counts.
func isPositiveInteger(v interface{}) bool {
	switch n := v.(type) {
	case int:
		return n > 0
	case int64:
		return n > 0
	case float64:
		return n >= 1 && n <= maxExactFloatInt && n == math.Trunc(n)
	default:
		return false
	}
}

// isSimilarityThreshold accepts a number in [0, 100]. YAML decodes 0 as int
// and 72.5 as float64, so both are numbers here.
func isSimilarityThreshold(v interface{}) bool {
	var f float64
	switch n := v.(type) {
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case float64:
		f = n
	default:
		return false
	}
	return f >= 0 && f <= 100
}

// isNonEmptyStringList rejects an empty list and blank entries: an empty
// keyword is a substring of every text, so it would match every target.
func isNonEmptyStringList(v interface{}) bool {
	var items []interface{}
	switch l := v.(type) {
	case []interface{}:
		items = l
	case []string:
		items = make([]interface{}, len(l))
		for i, s := range l {
			items[i] = s
		}
	default:
		return false
	}
	if len(items) == 0 {
		return false
	}
	for _, item := range items {
		if s, ok := item.(string); !ok || strings.TrimSpace(s) == "" {
			return false
		}
	}
	return true
}

// formatSettingValue quotes strings so a string "3" is distinguishable from
// the number 3 in an error message.
func formatSettingValue(v interface{}) string {
	if s, ok := v.(string); ok {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%v", v)
}

// validateConfidenceWeights checks each analysis.relationship.confidence_weights
// value is in (0, 1] (0 would zero every mapping that reports that level) and
// that a less confident level never weighs more than a more confident one.
// The order is checked only when every weight is in range, so one bad value
// is reported once.
func validateConfidenceWeights(w *RelationshipConfidenceWeights, tracker *sourceTracker) error {
	const pfx = "analysis.relationship.confidence_weights."
	var p problems
	levels := []struct {
		name  string
		value float64
	}{{"high", w.High}, {"medium", w.Medium}, {"low", w.Low}}
	for _, l := range levels {
		if math.IsNaN(l.value) || l.value <= 0 || l.value > 1 {
			p.add(pfx+l.name, pfx+"%s %g%s must be in range (0, 1]: %w",
				l.name, l.value, formatSource(tracker, pfx+l.name), ErrInvalidConfig)
		}
	}
	if len(p) > 0 {
		return p.err()
	}
	for i := 1; i < len(levels); i++ {
		more, less := levels[i-1], levels[i]
		if less.value > more.value {
			p.add(pfx+less.name, pfx+"%s %g%s must not exceed confidence_weights.%s %g: a %s vote cannot count for more than a %s one; "+
				"lower %s or raise %s: %w",
				less.name, less.value, formatSource(tracker, pfx+less.name), more.name, more.value,
				strings.ToUpper(less.name), strings.ToUpper(more.name), less.name, more.name, ErrInvalidConfig)
		}
	}
	return p.err()
}

// validateAnalyzerDependencies rejects an enabled analyzer that could not run
// as configured. The pipeline registers only enabled analyzers, so an enabled
// one whose dependency is disabled would leave the daemon unable to build its
// analyzer graph, and a pair analyzer with no candidate generator would
// analyse zero pairs and report success with no mappings. The dependencies
// mirror each analyzer's DependsOn (embedding on classify; requires and
// relationship on embedding), plus candidate generation, which reads the
// embeddings and runs the enabled generators before the pair analyzers.
// Each problem is reported under the enabled setting that imposes it.
func validateAnalyzerDependencies(a *AnalysisConfig, tracker *sourceTracker) error {
	var p problems
	if a.Embedding.Enabled && !a.Classification.Enabled {
		p.add("analysis.embedding.enabled", "analysis.embedding.enabled is true but analysis.classification.enabled is false%s: "+
			"the embedding analyzer runs after classification; enable analysis.classification or disable analysis.embedding: %w",
			formatSource(tracker, "analysis.embedding.enabled"), ErrInvalidConfig)
	}
	generatorEnabled := false
	for _, g := range a.Candidates.Generators {
		if g.Enabled {
			generatorEnabled = true
			break
		}
	}
	for _, pair := range []struct {
		name    string
		enabled bool
	}{{"relationship", a.Relationship.Enabled}, {"requires", a.Requires.Enabled}} {
		if !pair.enabled {
			continue
		}
		path := "analysis." + pair.name + ".enabled"
		if !a.Embedding.Enabled {
			p.add(path, "%s is true but analysis.embedding.enabled is false%s: the %s analyzer compares candidate pairs "+
				"generated from embeddings; enable analysis.embedding or disable analysis.%s: %w",
				path, formatSource(tracker, path), pair.name, pair.name, ErrInvalidConfig)
		}
		if !generatorEnabled {
			p.add(path, "%s is true but no analysis.candidates.generators entry is enabled%s: it would analyse zero candidate pairs "+
				"and the job would complete with no mappings; enable a generator (semantic, keyword or level) or disable analysis.%s: %w",
				path, formatSource(tracker, path), pair.name, ErrInvalidConfig)
		}
	}
	return p.err()
}

// validateCandidateGenerators checks every generator entry, enabled or not,
// so a mistake in a disabled entry does not surface only when it is enabled.
// The generators read these values at run time; without this check a typo'd
// key or out-of-range value would silently fall back to a default.
func validateCandidateGenerators(gens []CandidateGeneratorEntry, tracker *sourceTracker) error {
	var p problems
	source := formatSource(tracker, "analysis.candidates.generators")
	known := slices.Sorted(maps.Keys(candidateGeneratorSettings))
	for i, g := range gens {
		prefix := fmt.Sprintf("analysis.candidates.generators[%d]", i)
		if math.IsNaN(g.Weight) || g.Weight < 0 || g.Weight > 1 {
			p.add(prefix+".weight", "analysis.candidates.generators[%d].weight %g%s must be in range [0.0, 1.0]; 0 selects the default 1.0: %w",
				i, g.Weight, source, ErrInvalidConfig)
		}
		settings, ok := candidateGeneratorSettings[g.Name]
		if !ok {
			// An unknown generator has no settings to check its config against.
			p.add(prefix+".name", "analysis.candidates.generators[%d].name %q%s is not a known generator; use one of %v: %w",
				i, g.Name, source, known, ErrInvalidConfig)
			continue
		}
		for _, key := range slices.Sorted(maps.Keys(g.Config)) {
			setting, ok := settings[key]
			if !ok {
				allowed := "none"
				if len(settings) > 0 {
					allowed = fmt.Sprintf("%v", slices.Sorted(maps.Keys(settings)))
				}
				p.add(prefix+".config."+key, "analysis.candidates.generators[%d].config.%s%s is not a setting of generator %q; allowed keys: %s: %w",
					i, key, source, g.Name, allowed, ErrInvalidConfig)
				continue
			}
			if value := g.Config[key]; !setting.valid(value) {
				p.add(prefix+".config."+key, "analysis.candidates.generators[%d].config.%s %s%s %s: %w",
					i, key, formatSettingValue(value), source, setting.rule, ErrInvalidConfig)
			}
		}
	}
	return p.err()
}
