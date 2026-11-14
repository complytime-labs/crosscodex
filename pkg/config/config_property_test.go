package config_test

import (
	"errors"
	"reflect"
	"slices"
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

	Context("CritiqueConfig — panel alias uniqueness", func() {
		It("rejects a models list exactly when it repeats an alias", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				models := rapid.SliceOfN(rapid.SampledFrom([]string{"panel-1", "panel-2", "panel-3", "panel-4"}), 1, 6).Draw(t, "models")
				cfg := config.CritiqueConfig{
					Enabled: true, Models: models, SamplesPerModel: 1, ConsensusThreshold: 0.66,
					MaxErrorRate: 0.34, MaxSourceChars: 1, MaxTargetChars: 1, MaxTokens: 1,
				}
				err := cfg.Validate()
				repeats := len(slices.Compact(slices.Sorted(slices.Values(models)))) != len(models)
				if repeats && (err == nil || !strings.Contains(err.Error(), "twice")) {
					t.Fatalf("models %v repeat an alias but Validate returned %v", models, err)
				}
				if !repeats && err != nil {
					t.Fatalf("models %v are distinct but Validate returned %v", models, err)
				}
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
				cfg := validConfig()
				cfg.Attestation.ExpiryDuration = time.Duration(rapid.IntRange(1, 8760).Draw(t, "hours")) * time.Hour
				cfg.LLM = config.LLMConfig{
					GatewayMode: true,
					GatewayURL:  "",
					Timeout:     rapid.IntRange(0, 300).Draw(t, "timeout"),
					MaxRetries:  rapid.IntRange(0, 10).Draw(t, "maxRetries"),
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
				cfg := validConfig()
				cfg.Attestation.ExpiryDuration = time.Duration(rapid.IntRange(1, 8760).Draw(t, "hours")) * time.Hour
				cfg.LLM = config.LLMConfig{
					GatewayMode: true,
					GatewayURL:  url,
					Timeout:     rapid.IntRange(0, 300).Draw(t, "timeout"),
					MaxRetries:  rapid.IntRange(0, 10).Draw(t, "maxRetries"),
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
				cfg := validConfig()
				cfg.Attestation.ExpiryDuration = time.Duration(rapid.IntRange(1, 8760).Draw(t, "hours")) * time.Hour
				cfg.LLM = config.LLMConfig{
					GatewayMode: true,
					GatewayURL:  "http://litellm:4000",
					Timeout:     rapid.IntRange(0, 300).Draw(t, "timeout"),
					MaxRetries:  retries,
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

	Context("ResolveRole — fail-closed validation", func() {
		It("every canonical role and alias resolves without error, everything else fails", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				valid := map[string]bool{
					config.RoleAll: true, config.RoleGateway: true,
					config.RoleWorker: true, config.RoleGraph: true,
					"pipeline": true, "analysis": true, "synthesis": true,
				}
				candidate := rapid.StringMatching(`[a-z]{1,12}`).Draw(t, "candidate")

				_, err := config.ResolveRole(candidate)
				if valid[candidate] {
					if err != nil {
						t.Fatalf("expected %q to resolve, got error: %v", candidate, err)
					}
				} else if err == nil {
					t.Fatalf("expected %q to be rejected, got no error", candidate)
				}
			})
		})
	})
})

// Ensure imports are used.
var _ = strings.Join

// subsetNode returns a copy of n in which each mapping keeps a random subset
// of its pairs (recursively); sequences and scalars are copied whole.
func subsetNode(t *rapid.T, n *yaml.Node, label string) *yaml.Node {
	out := *n
	out.Content = nil
	if n.Kind != yaml.MappingNode {
		out.Content = append(out.Content, n.Content...)
		return &out
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i]
		if !rapid.Bool().Draw(t, label+"."+key.Value) {
			continue
		}
		k := *key
		out.Content = append(out.Content, &k, subsetNode(t, n.Content[i+1], label+"."+key.Value))
	}
	return &out
}

// structMappings appends to out every mapping in n that yaml.v3 decodes into
// a struct of type t without an inline map, so each of its keys must name a
// field; n is walked alongside t the way the decoder walks it.
func structMappings(n *yaml.Node, t reflect.Type, out *[]*yaml.Node) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t.Kind() == reflect.Struct && n.Kind == yaml.MappingNode:
		fields, inlineMap := config.ExportYAMLFields(t)
		if inlineMap == nil {
			*out = append(*out, n)
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			if ft, ok := fields[n.Content[i].Value]; ok {
				structMappings(n.Content[i+1], ft, out)
			}
		}
	case t.Kind() == reflect.Map && n.Kind == yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			structMappings(n.Content[i], t.Elem(), out)
		}
	case (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) && n.Kind == yaml.SequenceNode:
		for _, item := range n.Content {
			structMappings(item, t.Elem(), out)
		}
	}
}

var _ = Describe("Property Specifications", Ordered, func() {
	Context("checkKnownKeys — subsets of the defaults", func() {
		var defaults *yaml.Node

		BeforeAll(func() {
			// Every field of a default Config, including the ones the
			// compiled defaults YAML leaves out, encoded as yaml.v3 would.
			raw, err := config.ExportDefaultNode()
			Expect(err).NotTo(HaveOccurred())
			var cfg config.Config
			Expect(raw.Decode(&cfg)).To(Succeed())
			defaults = &yaml.Node{}
			Expect(defaults.Encode(&cfg)).To(Succeed())
		})

		It("accepts any subset, and rejects it once one key is renamed to an unknown name", func() {
			configType := reflect.TypeFor[config.Config]()
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				subset := subsetNode(t, defaults, "keep")
				if err := config.ExportCheckKnownKeys(subset, "subset.yaml"); err != nil {
					t.Fatalf("a subset of the defaults was rejected: %v", err)
				}

				var candidates []*yaml.Node
				structMappings(subset, configType, &candidates)
				candidates = slices.DeleteFunc(candidates, func(m *yaml.Node) bool { return len(m.Content) == 0 })
				if len(candidates) == 0 {
					return
				}
				target := candidates[rapid.IntRange(0, len(candidates)-1).Draw(t, "mapping")]
				pair := rapid.IntRange(0, len(target.Content)/2-1).Draw(t, "pair")
				unknown := "unknown_" + rapid.StringMatching(`[a-z]{1,8}`).Draw(t, "suffix")

				original := target.Content[2*pair]
				renamed := *original
				renamed.Value = unknown
				target.Content[2*pair] = &renamed
				err := config.ExportCheckKnownKeys(subset, "mutated.yaml")
				target.Content[2*pair] = original

				if !errors.Is(err, config.ErrUnknownKey) || !errors.Is(err, config.ErrInvalidConfig) {
					t.Fatalf("renaming %q to %q was not rejected as an unknown key: %v", original.Value, unknown, err)
				}
				if !strings.Contains(err.Error(), unknown) {
					t.Fatalf("rejection does not name the unknown key %q: %v", unknown, err)
				}
				if err := config.ExportCheckKnownKeys(subset, "subset.yaml"); err != nil {
					t.Fatalf("the subset was rejected after restoring the renamed key: %v", err)
				}
			})
		})
	})
})
