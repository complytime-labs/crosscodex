package main

import (
	"context"
	"os"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
)

var _ = Describe("resourcesForRole", func() {
	DescribeTable("declares exactly the shared resources each role needs",
		func(role string, want requiredResources) {
			Expect(resourcesForRole(role)).To(Equal(want))
		},
		Entry("graph needs db, graph, and nats but not llm",
			config.RoleGraph, requiredResources{db: true, graph: true, nats: true}),
		Entry("worker needs only nats and llm, never a database",
			config.RoleWorker, requiredResources{nats: true, llm: true}),
		Entry("pipeline needs db, nats, and llm",
			config.RolePipeline, requiredResources{db: true, nats: true, llm: true}),
		Entry("gateway needs only db, once pipeline is decoupled",
			config.RoleGateway, requiredResources{db: true}),
		Entry("all needs everything",
			config.RoleAll, requiredResources{db: true, graph: true, nats: true, llm: true}),
	)

	It("declares nothing for an unrecognized role", func() {
		Expect(resourcesForRole("unrecognized")).To(Equal(requiredResources{}))
	})
})

var _ = Describe("buildSharedResources attachNATS", func() {
	It("refuses embedded mode without starting a server on the store directory", func() {
		cfg := &config.Config{}
		cfg.NATS.Embedded.StoreDir = GinkgoT().TempDir()

		res, err := buildSharedResources(context.Background(), cfg, requiredResources{nats: true, attachNATS: true})

		Expect(res).To(BeNil())
		Expect(err).To(MatchError(natsbus.ErrExternalURLRequired))
		Expect(err.Error()).To(ContainSubstring("Set nats.url to the daemon's NATS server"))
		Expect(os.ReadDir(cfg.NATS.Embedded.StoreDir)).To(BeEmpty())
	})

	It("refuses a NATS server the daemon has not provisioned, instead of creating its streams", func() {
		srv, err := natsserver.NewServer(&natsserver.Options{
			Host: "127.0.0.1", Port: -1, JetStream: true,
			StoreDir: GinkgoT().TempDir(), NoLog: true, NoSigs: true,
		})
		Expect(err).NotTo(HaveOccurred())
		go srv.Start()
		DeferCleanup(srv.Shutdown)
		Expect(srv.ReadyForConnections(10 * time.Second)).To(BeTrue())
		cfg := &config.Config{}
		cfg.NATS.URL = srv.ClientURL()

		res, err := buildSharedResources(context.Background(), cfg, requiredResources{nats: true, attachNATS: true})

		Expect(res).To(BeNil())
		Expect(err).To(MatchError(natsbus.ErrStreamNotFound))
		info, err := srv.Jsz(nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Streams).To(BeZero())
	})
})
