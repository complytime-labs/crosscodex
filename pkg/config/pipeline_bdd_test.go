package config_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("pipeline.graph_materialize_timeout", func() {
	It("defaults to 30m", func() {
		_, cfg, err := loadConfigFile("")
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Pipeline.GraphMaterializeTimeout).To(Equal(30 * time.Minute))
		Expect(config.DefaultPipelineGraphMaterializeTimeout).To(Equal(30 * time.Minute))
	})

	It("accepts an override", func() {
		_, cfg, err := loadConfigFile("pipeline:\n  graph_materialize_timeout: 2h\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Pipeline.GraphMaterializeTimeout).To(Equal(2 * time.Hour))
	})

	It("accepts a timeout equal to pipeline.stage_timeout", func() {
		cfg := validConfig()
		cfg.Pipeline.GraphMaterializeTimeout = cfg.Pipeline.StageTimeout
		Expect(config.ExportValidateConfig(cfg)).To(Succeed())
	})

	DescribeTable("rejects a value outside (0, pipeline.stage_timeout], saying what to set",
		func(timeout time.Duration) {
			cfg := validConfig()
			cfg.Pipeline.StageTimeout = time.Hour
			cfg.Pipeline.GraphMaterializeTimeout = timeout
			err := config.ExportValidateConfig(cfg)
			Expect(err).To(MatchError(config.ErrInvalidConfig))
			Expect(err).To(MatchError(ContainSubstring("must be in range (0, pipeline.stage_timeout 1h0m0s]")))
			Expect(err).To(MatchError(ContainSubstring("such as 30m")))
			expectOnlyFieldError(err, "pipeline.graph_materialize_timeout")
		},
		Entry("zero", time.Duration(0)),
		Entry("negative", -time.Minute),
		Entry("above pipeline.stage_timeout", time.Hour+time.Second),
	)

	It("rejects 0 from a config file under its path", func() {
		_, _, err := loadConfigFile("pipeline:\n  graph_materialize_timeout: 0s\n")
		Expect(err).To(MatchError(config.ErrInvalidConfig))
		Expect(err.Error()).To(ContainSubstring("pipeline.graph_materialize_timeout"))
	})
})
