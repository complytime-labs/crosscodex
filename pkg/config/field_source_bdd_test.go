package config_test

import (
	"context"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("FieldError.Source", func() {
	sources := func(err error) map[string]string {
		out := map[string]string{}
		for _, fe := range config.FieldErrors(err) {
			out[fe.Path] = fe.Source
		}
		return out
	}

	It("names the file that set an invalid value, an unknown key or a wrong type", func() {
		file, _, err := loadConfigFile("logging:\n  level: loud\nloging: {}\npipeline:\n  max_concurrent_jobs: many\n")
		Expect(sources(err)).To(Equal(map[string]string{
			"logging.level":                file,
			"loging":                       file,
			"pipeline.max_concurrent_jobs": file,
		}))
	})

	It("names the file for a problem with a section, a quoted map key or a list item", func() {
		file, _, err := loadConfigFile("llm:\n  tenant_overrides:\n    BAD:\n      timeout: 5\n    \"a.b\":\n      timeout: 5\n" +
			"prompt:\n  layers:\n    order:\n      - id: nowhere\n")
		Expect(sources(err)).To(Equal(map[string]string{
			"llm.tenant_overrides.BAD":    file,
			`llm.tenant_overrides["a.b"]`: file,
			"prompt.layers.order[0].id":   file,
		}))
	})

	It("names the environment variable that set a value", func() {
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		GinkgoT().Setenv("CROSSCODEX_LOGGING_LEVEL", "loud")
		GinkgoT().Setenv("CROSSCODEX_PIPELINE_MAX_CONCURRENT_JOBS", "many")
		_, err := config.NewLoader().Load(context.Background())
		Expect(sources(err)).To(Equal(map[string]string{
			"logging.level":                "environment variable CROSSCODEX_LOGGING_LEVEL",
			"pipeline.max_concurrent_jobs": "environment variable CROSSCODEX_PIPELINE_MAX_CONCURRENT_JOBS",
		}))
	})

	It("names the compiled defaults for a missing value that another setting requires", func() {
		GinkgoT().Setenv("XDG_CONFIG_HOME", GinkgoT().TempDir())
		GinkgoT().Setenv("CROSSCODEX_LLM_GATEWAY_MODE", "true")
		_, err := config.NewLoader().Load(context.Background())
		Expect(sources(err)).To(Equal(map[string]string{"llm.gateway_url": "compiled defaults"}))
	})

	It("names the layer that set the backend for a backup setting the backend requires", func() {
		xdg := GinkgoT().TempDir()
		GinkgoT().Setenv("XDG_CONFIG_HOME", xdg)
		dropIn := filepath.Join(xdg, "crosscodex", "conf.d", "50-backup.yaml")
		writeTestFile(dropIn, "backup:\n  destination:\n    backend: s3\n  dsn: \"postgres://u@h/d\"\n")
		_, err := config.NewLoader().Load(context.Background())
		Expect(sources(err)).To(Equal(map[string]string{"backup.destination.s3.bucket": dropIn}))
	})

	It("is empty when validation runs without a tracker", func() {
		cfg := validConfig()
		cfg.Logging.Level = "loud"
		Expect(sources(config.ExportValidateConfig(cfg))).To(Equal(map[string]string{"logging.level": ""}))
	})
})
