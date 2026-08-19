package main

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("effectiveRole", func() {
	It("prefers the CLI flag over the loaded config value", func() {
		Expect(effectiveRole("worker", "all")).To(Equal("worker"))
	})

	It("falls back to the config value when the flag is unset", func() {
		Expect(effectiveRole("", "graph")).To(Equal("graph"))
	})
})
