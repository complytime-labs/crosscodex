package main

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/pipeline"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/llmclient"
	"github.com/complytime-labs/crosscodex/pkg/llmclient/llmclienttest"
	"github.com/complytime-labs/crosscodex/pkg/resultcache/resultcachetest"
)

// noCatalogClient is an llmclient.Client without a model catalog.
type noCatalogClient struct{ llmclient.Client }

var _ = Describe("llmProbeOptions", func() {
	It("wires the panel diversity probe, so the daemon refuses a panel that shares an upstream model", func() {
		shared := &sharedResources{probeClient: &llmclienttest.Fake{}}
		opts, err := llmProbeOptions(&config.Config{}, &resultcachetest.Fake{}, shared)
		Expect(err).NotTo(HaveOccurred())
		svc := pipeline.New(nil, nil, nil, nil, pipeline.Attestors{}, nil, nil, nil, config.PipelineConfig{}, nil, opts...)
		Expect(svc.PanelProbeWired()).To(BeTrue())
	})

	It("refuses a probe client that cannot read the gateway's model catalog, naming why", func() {
		shared := &sharedResources{probeClient: noCatalogClient{}}
		opts, err := llmProbeOptions(&config.Config{}, &resultcachetest.Fake{}, shared)
		Expect(opts).To(BeNil())
		Expect(err).To(MatchError(ContainSubstring("cannot read the gateway's model catalog")))
	})

	It("leaves the check unwired when no option wires it", func() {
		svc := pipeline.New(nil, nil, nil, nil, pipeline.Attestors{}, nil, nil, nil, config.PipelineConfig{}, nil)
		Expect(svc.PanelProbeWired()).To(BeFalse())
	})
})
