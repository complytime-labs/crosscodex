package config_test

import (
	"context"
	"math"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("Candidate generator configuration", func() {
	// withGenerator returns a valid Config whose only candidate generator is
	// entry, so each spec reaches that entry's validation.
	withGenerator := func(entry config.CandidateGeneratorEntry) *config.Config {
		cfg := validConfig()
		cfg.Analysis.Candidates.Generators = []config.CandidateGeneratorEntry{entry}
		return cfg
	}

	DescribeTable("rejects an invalid generator entry",
		func(entry config.CandidateGeneratorEntry, field, path, msg string) {
			err := config.ExportValidateConfig(withGenerator(entry))
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			Expect(err.Error()).To(ContainSubstring(field))
			Expect(err.Error()).To(ContainSubstring(msg))
			expectOnlyFieldError(err, path)
		},
		Entry("semantic top_k of zero",
			config.CandidateGeneratorEntry{Name: "semantic", Config: map[string]interface{}{"top_k": 0}},
			"analysis.candidates.generators[0].config.top_k 0", "analysis.candidates.generators[0].config.top_k", "must be a positive integer"),
		Entry("semantic fractional top_k",
			config.CandidateGeneratorEntry{Name: "semantic", Config: map[string]interface{}{"top_k": 1.5}},
			"analysis.candidates.generators[0].config.top_k 1.5", "analysis.candidates.generators[0].config.top_k", "must be a positive integer"),
		Entry("semantic string top_k",
			config.CandidateGeneratorEntry{Name: "semantic", Config: map[string]interface{}{"top_k": "3"}},
			`analysis.candidates.generators[0].config.top_k "3"`, "analysis.candidates.generators[0].config.top_k", "must be a positive integer"),
		Entry("semantic min_similarity above 100",
			config.CandidateGeneratorEntry{Name: "semantic", Config: map[string]interface{}{"min_similarity": 101}},
			"analysis.candidates.generators[0].config.min_similarity 101", "analysis.candidates.generators[0].config.min_similarity", "must be a number in range [0, 100]"),
		Entry("semantic negative min_similarity",
			config.CandidateGeneratorEntry{Name: "semantic", Config: map[string]interface{}{"min_similarity": -1}},
			"analysis.candidates.generators[0].config.min_similarity -1", "analysis.candidates.generators[0].config.min_similarity", "must be a number in range [0, 100]"),
		Entry("keyword empty keywords",
			config.CandidateGeneratorEntry{Name: "keyword", Config: map[string]interface{}{"keywords": []interface{}{}}},
			"analysis.candidates.generators[0].config.keywords", "analysis.candidates.generators[0].config.keywords", "must be a non-empty list of non-empty strings"),
		Entry("keyword blank keyword",
			config.CandidateGeneratorEntry{Name: "keyword", Config: map[string]interface{}{"keywords": []interface{}{""}}},
			"analysis.candidates.generators[0].config.keywords", "analysis.candidates.generators[0].config.keywords", "must be a non-empty list of non-empty strings"),
		Entry("keyword non-string keyword",
			config.CandidateGeneratorEntry{Name: "keyword", Config: map[string]interface{}{"keywords": []interface{}{7}}},
			"analysis.candidates.generators[0].config.keywords", "analysis.candidates.generators[0].config.keywords", "must be a non-empty list of non-empty strings"),
		Entry("semantic unknown key",
			config.CandidateGeneratorEntry{Name: "semantic", Config: map[string]interface{}{"topk": 3}},
			`analysis.candidates.generators[0].config.topk is not a setting of generator "semantic"`, "analysis.candidates.generators[0].config.topk", "allowed keys: [min_similarity top_k]"),
		Entry("level with any key",
			config.CandidateGeneratorEntry{Name: "level", Config: map[string]interface{}{"top_k": 3}},
			`analysis.candidates.generators[0].config.top_k is not a setting of generator "level"`, "analysis.candidates.generators[0].config.top_k", "allowed keys: none"),
		Entry("weight above 1",
			config.CandidateGeneratorEntry{Name: "semantic", Weight: 1.5},
			"analysis.candidates.generators[0].weight 1.5", "analysis.candidates.generators[0].weight", "must be in range [0.0, 1.0]"),
		Entry("negative weight",
			config.CandidateGeneratorEntry{Name: "level", Weight: -0.1},
			"analysis.candidates.generators[0].weight -0.1", "analysis.candidates.generators[0].weight", "must be in range [0.0, 1.0]"),
		Entry("NaN weight",
			config.CandidateGeneratorEntry{Name: "semantic", Weight: math.NaN()},
			"analysis.candidates.generators[0].weight NaN", "analysis.candidates.generators[0].weight", "must be in range [0.0, 1.0]"),
		Entry("semantic infinite top_k",
			config.CandidateGeneratorEntry{Name: "semantic", Config: map[string]interface{}{"top_k": math.Inf(1)}},
			"analysis.candidates.generators[0].config.top_k +Inf", "analysis.candidates.generators[0].config.top_k", "must be a positive integer"),
		Entry("semantic NaN top_k",
			config.CandidateGeneratorEntry{Name: "semantic", Config: map[string]interface{}{"top_k": math.NaN()}},
			"analysis.candidates.generators[0].config.top_k NaN", "analysis.candidates.generators[0].config.top_k", "must be a positive integer"),
		Entry("semantic top_k beyond exact float range",
			config.CandidateGeneratorEntry{Name: "semantic", Config: map[string]interface{}{"top_k": 1e300}},
			"analysis.candidates.generators[0].config.top_k 1e+300", "analysis.candidates.generators[0].config.top_k", "must be a positive integer"),
		Entry("semantic infinite min_similarity",
			config.CandidateGeneratorEntry{Name: "semantic", Config: map[string]interface{}{"min_similarity": math.Inf(1)}},
			"analysis.candidates.generators[0].config.min_similarity +Inf", "analysis.candidates.generators[0].config.min_similarity", "must be a number in range [0, 100]"),
		Entry("semantic NaN min_similarity",
			config.CandidateGeneratorEntry{Name: "semantic", Config: map[string]interface{}{"min_similarity": math.NaN()}},
			"analysis.candidates.generators[0].config.min_similarity NaN", "analysis.candidates.generators[0].config.min_similarity", "must be a number in range [0, 100]"),
		Entry("unknown generator name",
			config.CandidateGeneratorEntry{Name: "semantics"},
			`analysis.candidates.generators[0].name "semantics"`, "analysis.candidates.generators[0].name", "use one of [keyword level semantic]"),
	)

	DescribeTable("accepts a valid generator entry",
		func(entry config.CandidateGeneratorEntry) {
			Expect(config.ExportValidateConfig(withGenerator(entry))).To(Succeed())
		},
		Entry("semantic with integer top_k and min_similarity",
			config.CandidateGeneratorEntry{Name: "semantic", Enabled: true, Config: map[string]interface{}{"top_k": 3, "min_similarity": 0}}),
		Entry("semantic with fractional min_similarity",
			config.CandidateGeneratorEntry{Name: "semantic", Enabled: true, Config: map[string]interface{}{"min_similarity": 72.5}}),
		Entry("semantic with JSON-decoded integral top_k",
			config.CandidateGeneratorEntry{Name: "semantic", Enabled: true, Config: map[string]interface{}{"top_k": 3.0}}),
		Entry("keyword with keywords",
			config.CandidateGeneratorEntry{Name: "keyword", Enabled: true, Config: map[string]interface{}{"keywords": []interface{}{"policy"}}}),
		Entry("level with weight and no config",
			config.CandidateGeneratorEntry{Name: "level", Enabled: true, Weight: 0.5}),
		Entry("weight of 1",
			config.CandidateGeneratorEntry{Name: "semantic", Enabled: true, Weight: 1}),
	)

	It("validates the entry at its own index", func() {
		cfg := validConfig()
		cfg.Analysis.Candidates.Generators = []config.CandidateGeneratorEntry{
			{Name: "level", Enabled: true},
			{Name: "semantic", Enabled: true, Config: map[string]interface{}{"top_k": 0}},
		}
		Expect(config.ExportValidateConfig(cfg)).To(MatchError(ContainSubstring("analysis.candidates.generators[1].config.top_k 0")))
	})

	It("loads YAML generator config and names the file that set an invalid value", func() {
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		writeTestFile(path, "analysis:\n  candidates:\n    generators:\n"+
			"      - name: semantic\n        enabled: true\n        config:\n          top_k: 0\n")

		_, err := config.NewLoader().Load(context.Background(), config.WithConfigPath(path))
		Expect(err).To(MatchError(config.ErrInvalidConfig))
		Expect(err.Error()).To(ContainSubstring("analysis.candidates.generators[0].config.top_k 0 (set in " + path + ")"))
	})

	It("rejects a YAML .nan weight at load", func() {
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		writeTestFile(path, "analysis:\n  candidates:\n    generators:\n"+
			"      - name: level\n        enabled: true\n        weight: .nan\n")

		_, err := config.NewLoader().Load(context.Background(), config.WithConfigPath(path))
		Expect(err).To(MatchError(config.ErrInvalidConfig))
		Expect(err.Error()).To(ContainSubstring("analysis.candidates.generators[0].weight NaN"))
	})

	It("loads valid YAML generator config with its values intact", func() {
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
		writeTestFile(path, "analysis:\n  candidates:\n    generators:\n"+
			"      - name: semantic\n        enabled: true\n        weight: 0.5\n        config:\n          top_k: 3\n          min_similarity: 72.5\n"+
			"      - name: keyword\n        enabled: true\n        config:\n          keywords: [policy]\n")

		cfg, err := config.NewLoader().Load(context.Background(), config.WithConfigPath(path))
		Expect(err).NotTo(HaveOccurred())
		gens := cfg.Analysis.Candidates.Generators
		Expect(gens).To(HaveLen(2))
		Expect(gens[0].Weight).To(Equal(0.5))
		Expect(gens[0].Config).To(Equal(map[string]interface{}{"top_k": 3, "min_similarity": 72.5}))
		Expect(gens[1].Config).To(Equal(map[string]interface{}{"keywords": []interface{}{"policy"}}))
	})
})
