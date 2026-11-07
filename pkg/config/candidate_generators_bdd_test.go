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
		Entry("lexical top_k of zero",
			config.CandidateGeneratorEntry{Name: "lexical", Config: map[string]interface{}{"top_k": 0}},
			"analysis.candidates.generators[0].config.top_k 0", "analysis.candidates.generators[0].config.top_k", "must be a positive integer"),
		Entry("lexical unknown key",
			config.CandidateGeneratorEntry{Name: "lexical", Config: map[string]interface{}{"topk": 3}},
			`analysis.candidates.generators[0].config.topk is not a setting of generator "lexical"`, "analysis.candidates.generators[0].config.topk", "allowed keys: [top_k]"),
		Entry("unknown generator name",
			config.CandidateGeneratorEntry{Name: "semantics"},
			`analysis.candidates.generators[0].name "semantics"`, "analysis.candidates.generators[0].name", "use one of [keyword level lexical semantic]"),
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
		Entry("lexical with integer top_k",
			config.CandidateGeneratorEntry{Name: "lexical", Enabled: true, Config: map[string]interface{}{"top_k": 3}}),
		Entry("enabled semantic with top_k equal to max_per_source",
			config.CandidateGeneratorEntry{Name: "semantic", Enabled: true, Config: map[string]interface{}{"top_k": 10}}),
		Entry("disabled semantic with top_k above max_per_source",
			config.CandidateGeneratorEntry{Name: "semantic", Enabled: false, Config: map[string]interface{}{"top_k": 50}}),
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

	Describe("max_per_source", func() {
		const maxPath = "analysis.candidates.max_per_source"

		DescribeTable("rejects a value below 1",
			func(value int) {
				cfg := validConfig()
				cfg.Analysis.Candidates.MaxPerSource = value
				err := config.ExportValidateConfig(cfg)
				Expect(err).To(MatchError(config.ErrInvalidConfig))
				Expect(err.Error()).To(ContainSubstring(maxPath))
				Expect(err.Error()).To(ContainSubstring("must be at least 1"))
				expectOnlyFieldError(err, maxPath)
			},
			Entry("zero", 0),
			Entry("negative", -1),
		)

		It("accepts a valid override", func() {
			cfg := validConfig()
			cfg.Analysis.Candidates.MaxPerSource = 1
			Expect(config.ExportValidateConfig(cfg)).To(Succeed())
		})

		DescribeTable("rejects an enabled generator whose top_k exceeds the cap",
			func(name string, topK interface{}) {
				cfg := withGenerator(config.CandidateGeneratorEntry{
					Name: name, Enabled: true, Config: map[string]interface{}{"top_k": topK}})
				cfg.Analysis.Candidates.MaxPerSource = 10
				err := config.ExportValidateConfig(cfg)
				Expect(err).To(MatchError(config.ErrInvalidConfig))
				Expect(err.Error()).To(ContainSubstring("max_per_source"))
				Expect(err.Error()).To(ContainSubstring("raise analysis.candidates.max_per_source or lower"))
				expectOnlyFieldError(err, "analysis.candidates.generators[0].config.top_k")
			},
			Entry("semantic int", "semantic", 11),
			Entry("semantic JSON float", "semantic", float64(11)),
			Entry("lexical int", "lexical", 11),
			Entry("lexical JSON float", "lexical", float64(11)),
		)

		It("rejects an enabled generator relying on the default top_k of 10", func() {
			cfg := withGenerator(config.CandidateGeneratorEntry{Name: "lexical", Enabled: true})
			cfg.Analysis.Candidates.MaxPerSource = 5
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			Expect(err.Error()).To(ContainSubstring("default 10"))
			expectOnlyFieldError(err, "analysis.candidates.generators[0]")
		})

		It("reports only max_per_source when it is invalid, not every generator", func() {
			cfg := withGenerator(config.CandidateGeneratorEntry{Name: "semantic", Enabled: true})
			cfg.Analysis.Candidates.MaxPerSource = 0
			expectOnlyFieldError(config.ExportValidateConfig(cfg), maxPath)
		})

		It("defaults to 10 in the compiled defaults", func() {
			GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
			cfg, err := config.NewLoader().Load(context.Background())
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Analysis.Candidates.MaxPerSource).To(Equal(10))
		})

		It("loads a YAML override and names the file that set an invalid value", func() {
			GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
			path := filepath.Join(GinkgoT().TempDir(), "config.yaml")
			writeTestFile(path, "analysis:\n  candidates:\n    max_per_source: 0\n")
			_, err := config.NewLoader().Load(context.Background(), config.WithConfigPath(path))
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			Expect(err.Error()).To(ContainSubstring(maxPath + " 0 (set in " + path + ")"))

			writeTestFile(path, "analysis:\n  candidates:\n    max_per_source: 25\n")
			cfg, err := config.NewLoader().Load(context.Background(), config.WithConfigPath(path))
			Expect(err).NotTo(HaveOccurred())
			Expect(cfg.Analysis.Candidates.MaxPerSource).To(Equal(25))
		})
	})
})
