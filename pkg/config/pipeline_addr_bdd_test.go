package config_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("PipelineConfig.Addr and Endpoint", func() {
	It("default to empty strings", func() {
		cfg, err := config.NewLoader().Load(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Pipeline.Addr).To(Equal(""))
		Expect(cfg.Pipeline.Endpoint).To(Equal(""))
	})

	It("load from environment variables", func() {
		GinkgoT().Setenv("CROSSCODEX_PIPELINE_ADDR", ":8082")
		GinkgoT().Setenv("CROSSCODEX_PIPELINE_ENDPOINT", "pipeline.internal:8082")
		cfg, err := config.NewLoader().Load(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Pipeline.Addr).To(Equal(":8082"))
		Expect(cfg.Pipeline.Endpoint).To(Equal("pipeline.internal:8082"))
	})
})
