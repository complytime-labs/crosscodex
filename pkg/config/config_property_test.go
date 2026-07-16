package config_test

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"gopkg.in/yaml.v3"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

// Suite bootstrap lives in config_bdd_test.go (TestConfigBDD).
// This file only registers Describe nodes; Ginkgo collects them automatically.

// marshalNode marshals a yaml.Node to a string for comparison.
func marshalNode(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	// Wrap in a document node for marshalling
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{n}}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "<marshal-error: " + err.Error() + ">"
	}
	return string(out)
}

// genYAMLMappingNode generates a random YAML mapping node with 1-5 scalar key-value pairs.
func genYAMLMappingNode(t *rapid.T) *yaml.Node {
	numPairs := rapid.IntRange(1, 5).Draw(t, "numPairs")
	content := make([]*yaml.Node, 0, numPairs*2)
	seen := make(map[string]bool)
	for i := 0; i < numPairs; i++ {
		key := rapid.StringMatching(`[a-z][a-z0-9_]{0,9}`).Draw(t, "key")
		if seen[key] {
			continue // skip duplicate keys
		}
		seen[key] = true
		val := rapid.StringMatching(`[a-zA-Z0-9_.-]{0,20}`).Draw(t, "val")
		content = append(content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"},
			&yaml.Node{Kind: yaml.ScalarNode, Value: val, Tag: "!!str"},
		)
	}
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: content}
}

var _ = Describe("Property Specifications", Ordered, func() {

	Context("deepMerge — nil overlay returns base", func() {
		It("deepMerge(a, nil) equals cloneNode(a)", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				base := genYAMLMappingNode(t)
				result, err := config.ExportDeepMerge(base, nil)
				Expect(err).NotTo(HaveOccurred())
				Expect(marshalNode(result)).To(Equal(marshalNode(config.ExportCloneNode(base))),
					"deepMerge(base, nil) should equal cloneNode(base)")
			})
		})
	})

	Context("deepMerge — overlay wins for scalars", func() {
		It("overlay value overrides base for the same key", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				key := rapid.StringMatching(`[a-z]{1,8}`).Draw(t, "key")
				baseVal := rapid.StringMatching(`[a-z]{1,10}`).Draw(t, "baseVal")
				overlayVal := rapid.StringMatching(`[a-z]{1,10}`).Draw(t, "overlayVal")

				base := &yaml.Node{
					Kind: yaml.MappingNode, Tag: "!!map",
					Content: []*yaml.Node{
						{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"},
						{Kind: yaml.ScalarNode, Value: baseVal, Tag: "!!str"},
					},
				}
				overlay := &yaml.Node{
					Kind: yaml.MappingNode, Tag: "!!map",
					Content: []*yaml.Node{
						{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"},
						{Kind: yaml.ScalarNode, Value: overlayVal, Tag: "!!str"},
					},
				}

				result, err := config.ExportDeepMerge(base, overlay)
				Expect(err).NotTo(HaveOccurred())

				// Find the key in the result and verify the overlay value won
				found := false
				for i := 0; i < len(result.Content)-1; i += 2 {
					if result.Content[i].Value == key {
						Expect(result.Content[i+1].Value).To(Equal(overlayVal),
							"overlay value should override base for key %q", key)
						found = true
						break
					}
				}
				Expect(found).To(BeTrue(), "merged result should contain key %q", key)
			})
		})
	})

	Context("cloneNode — deep equality", func() {
		It("clone produces identical YAML output", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				node := genYAMLMappingNode(t)
				clone := config.ExportCloneNode(node)
				Expect(marshalNode(clone)).To(Equal(marshalNode(node)),
					"cloned node should marshal identically")
			})
		})
	})

	Context("cloneNode — isolation", func() {
		It("mutating clone does not affect original", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				node := genYAMLMappingNode(t)
				originalYAML := marshalNode(node)

				clone := config.ExportCloneNode(node)
				// Mutate the clone: change the first value node
				if len(clone.Content) >= 2 {
					clone.Content[1].Value = "MUTATED"
				}

				Expect(marshalNode(node)).To(Equal(originalYAML),
					"original node should be unaffected by clone mutation")
			})
		})
	})

	Context("buildNodeSimple — roundtrip", func() {
		It("produces a mapping node at the given path", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				numSegments := rapid.IntRange(1, 4).Draw(t, "numSegments")
				segments := make([]string, numSegments)
				for i := range segments {
					segments[i] = rapid.StringMatching(`[a-z]{1,8}`).Draw(t, "segment")
				}
				value := rapid.StringMatching(`[a-zA-Z0-9]{1,15}`).Draw(t, "value")

				node := config.ExportBuildNodeSimple(segments, value)
				Expect(node).NotTo(BeNil())
				Expect(node.Kind).To(Equal(yaml.MappingNode))

				// Walk down the nesting to find the leaf value
				current := node
				for i := 0; i < numSegments; i++ {
					Expect(current.Kind).To(Equal(yaml.MappingNode),
						"node at depth %d should be a mapping", i)
					Expect(len(current.Content)).To(BeNumerically(">=", 2))
					Expect(current.Content[0].Value).To(Equal(segments[i]),
						"key at depth %d should match segment", i)
					current = current.Content[1]
				}
				Expect(current.Kind).To(Equal(yaml.ScalarNode))
				Expect(current.Value).To(Equal(value))
			})
		})
	})

	Context("validate — rejects invalid TLS mode", func() {
		It("rejects uppercase random strings as TLS mode", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				mode := rapid.StringMatching(`[A-Z]{1,10}`).Draw(t, "mode")
				cfg := &config.Config{}
				cfg.TLS.Mode = mode
				cfg.Attestation.ExpiryDuration = 8760 * time.Hour
				err := config.ExportValidateConfig(cfg)
				Expect(err).To(HaveOccurred(),
					"validate should reject TLS mode %q", mode)
				Expect(err.Error()).To(ContainSubstring("tls.mode"))
			})
		})
	})

	Context("validate — rejects invalid storage backend", func() {
		It("rejects uppercase random strings as storage backend", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				backend := rapid.StringMatching(`[A-Z]{1,10}`).Draw(t, "backend")
				cfg := &config.Config{}
				cfg.Storage.Objects.Backend = backend
				cfg.Attestation.ExpiryDuration = 8760 * time.Hour
				err := config.ExportValidateConfig(cfg)
				Expect(err).To(HaveOccurred(),
					"validate should reject storage backend %q", backend)
				Expect(err.Error()).To(ContainSubstring("storage.objects.backend"))
			})
		})
	})

	Context("ForTenant — returns global config when no override", func() {
		It("all 5 fields match global values for unknown tenant", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				tenantID := rapid.StringMatching(`[a-z]{3,12}`).Draw(t, "tenantID")
				enabled := rapid.Bool().Draw(t, "enabled")
				privKey := rapid.StringMatching(`/tmp/[a-z]{3,8}\.pem`).Draw(t, "privKey")
				pubKey := rapid.StringMatching(`/tmp/[a-z]{3,8}\.pub`).Draw(t, "pubKey")
				hours := rapid.IntRange(1, 8760).Draw(t, "hours")
				expiry := time.Duration(hours) * time.Hour
				byProducts := rapid.Bool().Draw(t, "byProducts")

				ac := config.AttestationConfig{
					Enabled:           enabled,
					PrivateKeyPath:    privKey,
					PublicKeyPath:     pubKey,
					ExpiryDuration:    expiry,
					IncludeByProducts: byProducts,
				}

				tc := ac.ForTenant(tenantID)
				Expect(tc.Enabled).To(Equal(enabled))
				Expect(tc.PrivateKeyPath).To(Equal(privKey))
				Expect(tc.PublicKeyPath).To(Equal(pubKey))
				Expect(tc.ExpiryDuration).To(Equal(expiry))
				Expect(tc.IncludeByProducts).To(Equal(byProducts))
			})
		})
	})

	Context("LLMConfig.ForTenant — returns global config when no override", func() {
		It("all fields match global values for unknown tenant", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				tenantID := rapid.StringMatching(`[a-z]{3,12}`).Draw(t, "tenantID")
				gatewayURL := rapid.StringMatching(`https?://[a-z]{3,8}:\d{4}`).Draw(t, "gatewayURL")
				gatewayMode := rapid.Bool().Draw(t, "gatewayMode")
				defaultModel := rapid.StringMatching(`[a-z]{3,10}`).Draw(t, "defaultModel")
				embeddingModel := rapid.StringMatching(`[a-z]{3,10}`).Draw(t, "embeddingModel")
				apiKeyRef := rapid.StringMatching(`env://[A-Z_]{3,10}`).Draw(t, "apiKeyRef")
				maxRetries := rapid.IntRange(0, 10).Draw(t, "maxRetries")
				timeout := rapid.IntRange(0, 300).Draw(t, "timeout")

				lc := config.LLMConfig{
					GatewayURL:     gatewayURL,
					GatewayMode:    gatewayMode,
					DefaultModel:   defaultModel,
					EmbeddingModel: embeddingModel,
					APIKeyRef:      apiKeyRef,
					AllowedModels:  []string{"model-a", "model-b"},
					MaxRetries:     maxRetries,
					Timeout:        timeout,
				}

				tc := lc.ForTenant(tenantID)
				Expect(tc.GatewayURL).To(Equal(gatewayURL))
				Expect(tc.GatewayMode).To(Equal(gatewayMode))
				Expect(tc.DefaultModel).To(Equal(defaultModel))
				Expect(tc.EmbeddingModel).To(Equal(embeddingModel))
				Expect(tc.APIKeyRef).To(Equal(apiKeyRef))
				Expect(tc.AllowedModels).To(Equal([]string{"model-a", "model-b"}))
				Expect(tc.MaxRetries).To(Equal(maxRetries))
				Expect(tc.Timeout).To(Equal(timeout))
			})
		})
	})

	Context("LLMConfig.ForTenant — override wins", func() {
		It("override values take precedence over global", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				tenantID := rapid.StringMatching(`[a-z]{3,12}`).Draw(t, "tenantID")

				// Global values
				globalURL := rapid.StringMatching(`https?://global[a-z]{1,4}:\d{4}`).Draw(t, "globalURL")
				globalModel := rapid.StringMatching(`global-[a-z]{3,6}`).Draw(t, "globalModel")
				globalEmbModel := rapid.StringMatching(`global-emb-[a-z]{3,6}`).Draw(t, "globalEmbModel")
				globalKeyRef := rapid.StringMatching(`env://GLOBAL_[A-Z]{3,6}`).Draw(t, "globalKeyRef")
				globalRetries := rapid.IntRange(0, 10).Draw(t, "globalRetries")
				globalTimeout := rapid.IntRange(0, 300).Draw(t, "globalTimeout")

				// Override values
				overrideURL := rapid.StringMatching(`https?://override[a-z]{1,4}:\d{4}`).Draw(t, "overrideURL")
				overrideModel := rapid.StringMatching(`override-[a-z]{3,6}`).Draw(t, "overrideModel")
				overrideEmbModel := rapid.StringMatching(`override-emb-[a-z]{3,6}`).Draw(t, "overrideEmbModel")
				overrideKeyRef := rapid.StringMatching(`env://OVER_[A-Z]{3,6}`).Draw(t, "overrideKeyRef")
				overrideRetries := rapid.IntRange(0, 10).Draw(t, "overrideRetries")
				overrideTimeout := rapid.IntRange(0, 300).Draw(t, "overrideTimeout")
				overrideModels := []string{"override-model-x"}

				lc := config.LLMConfig{
					GatewayURL:     globalURL,
					DefaultModel:   globalModel,
					EmbeddingModel: globalEmbModel,
					APIKeyRef:      globalKeyRef,
					AllowedModels:  []string{"global-model-a"},
					MaxRetries:     globalRetries,
					Timeout:        globalTimeout,
					TenantOverrides: map[string]config.LLMOverride{
						tenantID: {
							GatewayURL:     &overrideURL,
							DefaultModel:   &overrideModel,
							EmbeddingModel: &overrideEmbModel,
							APIKeyRef:      &overrideKeyRef,
							AllowedModels:  overrideModels,
							MaxRetries:     &overrideRetries,
							Timeout:        &overrideTimeout,
						},
					},
				}

				tc := lc.ForTenant(tenantID)
				Expect(tc.GatewayURL).To(Equal(overrideURL))
				Expect(tc.DefaultModel).To(Equal(overrideModel))
				Expect(tc.EmbeddingModel).To(Equal(overrideEmbModel))
				Expect(tc.APIKeyRef).To(Equal(overrideKeyRef))
				Expect(tc.AllowedModels).To(Equal(overrideModels))
				Expect(tc.MaxRetries).To(Equal(overrideRetries))
				Expect(tc.Timeout).To(Equal(overrideTimeout))
			})
		})
	})

	Context("validate — gateway_mode=true always requires gateway_url", func() {
		It("rejects every config where gateway_mode is true and gateway_url is empty", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				cfg := &config.Config{
					TLS:     config.TLSConfig{Mode: "off"},
					Storage: config.StorageConfig{Objects: config.ObjectStorageConfig{Backend: "local"}},
					Logging: config.LoggingConfig{Level: "info", Format: "text"},
					Attestation: config.AttestationConfig{
						Enabled:        true,
						ExpiryDuration: time.Duration(rapid.IntRange(1, 8760).Draw(t, "hours")) * time.Hour,
					},
					Analysis: config.AnalysisConfig{
						Engine:         config.EngineConfig{TaskTimeout: 5 * time.Minute, MaxRetries: 3, RetryBackoff: time.Second},
						Classification: config.ClassificationConfig{MaxTextLength: 2000, MaxTokens: 20},
						Embedding:      config.EmbeddingConfig{Enabled: true, Models: []string{"m"}, MaxChars: 1500, BatchSize: 50},
						Relationship:   config.RelationshipConfig{TopK: 20, MaxSourceChars: 1500, MaxTargetChars: 800, MaxTokens: 300, SamplesPerModel: 1, SamplingTemperature: 0.3},
					},
					LLM: config.LLMConfig{
						GatewayMode: true,
						GatewayURL:  "",
						Timeout:     rapid.IntRange(0, 300).Draw(t, "timeout"),
						MaxRetries:  rapid.IntRange(0, 10).Draw(t, "maxRetries"),
					},
				}
				err := config.ExportValidateConfig(cfg)
				Expect(err).To(HaveOccurred(),
					"validate should reject gateway_mode=true with empty gateway_url")
				Expect(err.Error()).To(ContainSubstring("gateway_url"))
			})
		})

		It("accepts every config where gateway_mode is true and gateway_url is non-empty", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				url := rapid.StringMatching(`https?://[a-z]{3,10}:\d{4}`).Draw(t, "url")
				cfg := &config.Config{
					TLS:     config.TLSConfig{Mode: "off"},
					Storage: config.StorageConfig{Objects: config.ObjectStorageConfig{Backend: "local"}},
					Logging: config.LoggingConfig{Level: "info", Format: "text"},
					Attestation: config.AttestationConfig{
						Enabled:        true,
						ExpiryDuration: time.Duration(rapid.IntRange(1, 8760).Draw(t, "hours")) * time.Hour,
					},
					Analysis: config.AnalysisConfig{
						Engine:         config.EngineConfig{TaskTimeout: 5 * time.Minute, MaxRetries: 3, RetryBackoff: time.Second},
						Classification: config.ClassificationConfig{MaxTextLength: 2000, MaxTokens: 20},
						Embedding:      config.EmbeddingConfig{Enabled: true, Models: []string{"m"}, MaxChars: 1500, BatchSize: 50},
						Relationship:   config.RelationshipConfig{TopK: 20, MaxSourceChars: 1500, MaxTargetChars: 800, MaxTokens: 300, SamplesPerModel: 1, SamplingTemperature: 0.3},
					},
					Synthesis: config.SynthesisConfig{
						ConfidenceThreshold:   0.5,
						MaxMappingsPerControl: 10,
						Viability:             config.ViabilityConfig{TypeMismatchFactor: 0.8, SkipLevelFactor: 0.7, IntegralToFactor: 1.1},
						Assessment:            config.AssessmentConfig{IQRGood: 20, IQRPoor: 10, NoRelHigh: 0.97, NoRelLow: 0.80, ContestedWarn: 0.20, ActionableWarn: 0.30},
					},
					LLM: config.LLMConfig{
						GatewayMode: true,
						GatewayURL:  url,
						Timeout:     rapid.IntRange(0, 300).Draw(t, "timeout"),
						MaxRetries:  rapid.IntRange(0, 10).Draw(t, "maxRetries"),
					},
				}
				err := config.ExportValidateConfig(cfg)
				Expect(err).NotTo(HaveOccurred(),
					"validate should accept gateway_mode=true with gateway_url=%q", url)
			})
		})
	})

	Context("validate — gateway_mode=true always zeros max_retries", func() {
		It("sets max_retries to 0 after validation when gateway_mode is true", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				retries := rapid.IntRange(1, 100).Draw(t, "retries")
				cfg := &config.Config{
					TLS:     config.TLSConfig{Mode: "off"},
					Storage: config.StorageConfig{Objects: config.ObjectStorageConfig{Backend: "local"}},
					Logging: config.LoggingConfig{Level: "info", Format: "text"},
					Attestation: config.AttestationConfig{
						Enabled:        true,
						ExpiryDuration: time.Duration(rapid.IntRange(1, 8760).Draw(t, "hours")) * time.Hour,
					},
					Analysis: config.AnalysisConfig{
						Engine:         config.EngineConfig{TaskTimeout: 5 * time.Minute, MaxRetries: 3, RetryBackoff: time.Second},
						Classification: config.ClassificationConfig{MaxTextLength: 2000, MaxTokens: 20},
						Embedding:      config.EmbeddingConfig{Enabled: true, Models: []string{"m"}, MaxChars: 1500, BatchSize: 50},
						Relationship:   config.RelationshipConfig{TopK: 20, MaxSourceChars: 1500, MaxTargetChars: 800, MaxTokens: 300, SamplesPerModel: 1, SamplingTemperature: 0.3},
					},
					Synthesis: config.SynthesisConfig{
						ConfidenceThreshold:   0.5,
						MaxMappingsPerControl: 10,
						Viability:             config.ViabilityConfig{TypeMismatchFactor: 0.8, SkipLevelFactor: 0.7, IntegralToFactor: 1.1},
						Assessment:            config.AssessmentConfig{IQRGood: 20, IQRPoor: 10, NoRelHigh: 0.97, NoRelLow: 0.80, ContestedWarn: 0.20, ActionableWarn: 0.30},
					},
					LLM: config.LLMConfig{
						GatewayMode: true,
						GatewayURL:  "http://litellm:4000",
						Timeout:     rapid.IntRange(0, 300).Draw(t, "timeout"),
						MaxRetries:  retries,
					},
				}
				err := config.ExportValidateConfig(cfg)
				Expect(err).NotTo(HaveOccurred())
				Expect(cfg.LLM.MaxRetries).To(Equal(0),
					"max_retries should be zeroed when gateway_mode=true, was %d", retries)
			})
		})
	})

	Context("deepMerge — nil base returns overlay", func() {
		It("deepMerge(nil, b) equals cloneNode(b)", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				overlay := genYAMLMappingNode(t)
				result, err := config.ExportDeepMerge(nil, overlay)
				Expect(err).NotTo(HaveOccurred())
				Expect(marshalNode(result)).To(Equal(marshalNode(config.ExportCloneNode(overlay))),
					"deepMerge(nil, overlay) should equal cloneNode(overlay)")
			})
		})
	})
})

// Ensure imports are used.
var _ = strings.Join
