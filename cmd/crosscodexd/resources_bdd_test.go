package main

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
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
		Entry("gateway needs db, nats, and llm but not graph",
			config.RoleGateway, requiredResources{db: true, nats: true, llm: true}),
		Entry("all needs everything",
			config.RoleAll, requiredResources{db: true, graph: true, nats: true, llm: true}),
	)

	It("declares nothing for an unrecognized role", func() {
		Expect(resourcesForRole("unrecognized")).To(Equal(requiredResources{}))
	})
})
