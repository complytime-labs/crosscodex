package config_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("Values of the wrong type", func() {
	paths := func(err error) []string {
		var out []string
		for _, fe := range config.FieldErrors(err) {
			out = append(out, fe.Path)
		}
		return out
	}

	DescribeTable("are reported against the file and line that set them, tagged with the setting",
		func(content, path, position, detail string) {
			file, _, err := loadConfigFile(content)
			Expect(err).To(MatchError(config.ErrWrongType))
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			Expect(err).To(MatchError(config.ErrLoadFailed))
			Expect(err).To(MatchError(ContainSubstring(path + " at " + file + ":" + position)))
			Expect(err).To(MatchError(ContainSubstring(detail)))
			Expect(err.Error()).NotTo(MatchRegexp(`line \d+:`))
			expectOnlyFieldError(err, path)
		},
		Entry("text for a number", "pipeline:\n  max_concurrent_jobs: many\n",
			"pipeline.max_concurrent_jobs", "2:24", "cannot unmarshal !!str `many` into int"),
		Entry("text for a duration", "pipeline:\n  stage_timeout: soon\n",
			"pipeline.stage_timeout", "2:18", "into time.Duration"),
		Entry("text for a bool", "llm:\n  gateway_mode: maybe\n",
			"llm.gateway_mode", "2:17", "into bool"),
		Entry("a value where a section belongs", "logging: debug\n",
			"logging", "1:10", "into config.LoggingConfig"),
		Entry("a value where a list belongs", "llm:\n  allowed_models: granite\n",
			"llm.allowed_models", "2:19", "into []string"),
		Entry("a list where a value belongs", "llm:\n  default_model: [a, b]\n",
			"llm.default_model", "2:18", "into string"),
		Entry("a wrong type in a tenant override", "llm:\n  tenant_overrides:\n    acme-corp:\n      timeout: soon\n",
			"llm.tenant_overrides.acme-corp.timeout", "4:16", "into int"),
		Entry("a wrong type reached through a merge key", "llm:\n  tenant_overrides:\n    acme-corp: &o\n      timeout: soon\n    beta-inc:\n      <<: *o\n",
			"llm.tenant_overrides.acme-corp.timeout", "4:16", "into int"),
	)

	It("is reported together with invalid values and unknown keys, each once", func() {
		_, _, err := loadConfigFile("pipeline:\n  max_concurrent_jobs: many\n  stage_timeout: soon\nlogging:\n  level: loud\nloging: {}\n")
		Expect(paths(err)).To(ConsistOf("pipeline.max_concurrent_jobs", "pipeline.stage_timeout", "logging.level", "loging"))
		Expect(config.FieldErrorsComplete(err)).To(BeTrue())
	})

	It("is reported for an environment variable, naming it", func() {
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		GinkgoT().Setenv("CROSSCODEX_PIPELINE_MAX_CONCURRENT_JOBS", "many")
		_, err := config.NewLoader().Load(context.Background())
		Expect(err).To(MatchError(config.ErrWrongType))
		Expect(err).To(MatchError(ContainSubstring("at environment variable CROSSCODEX_PIPELINE_MAX_CONCURRENT_JOBS")))
		expectOnlyFieldError(err, "pipeline.max_concurrent_jobs")
	})

	It("reads CLI flag overrides as YAML scalars, as a file would", func() {
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		cfg, err := config.NewLoader().Load(context.Background(), config.WithOverrides(map[string]string{
			"pipeline.max_concurrent_jobs": "8",
			"pipeline.stage_timeout":       "2h",
			"llm.gateway_mode":             "true",
			"llm.gateway_url":              "http://litellm:4000",
			"llm.default_model":            "granite",
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Pipeline.MaxConcurrentJobs).To(Equal(8))
		Expect(cfg.Pipeline.StageTimeout.String()).To(Equal("2h0m0s"))
		Expect(cfg.LLM.GatewayMode).To(BeTrue())
		Expect(cfg.LLM.DefaultModel).To(Equal("granite"))
	})

	It("is reported for a CLI flag override, naming it", func() {
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		_, err := config.NewLoader().Load(context.Background(),
			config.WithOverrides(map[string]string{"pipeline.max_concurrent_jobs": "many"}))
		Expect(err).To(MatchError(config.ErrWrongType))
		Expect(err).To(MatchError(ContainSubstring("at CLI flag pipeline.max_concurrent_jobs")))
		expectOnlyFieldError(err, "pipeline.max_concurrent_jobs")
	})
})
