package natsbus_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
)

var _ = Describe("Client.Connected", func() {
	It("is true while the server is up, false once it goes away, and false after Close", func() {
		srv := startExternalServer()
		c, err := natsbus.New(config.NATSConfig{URL: srv.ClientURL()})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(c.Close)

		Expect(c.Connected()).To(BeTrue())

		srv.Shutdown()
		Eventually(c.Connected).WithTimeout(10 * time.Second).Should(BeFalse())

		Expect(c.Close()).To(Succeed())
		Expect(c.Connected()).To(BeFalse())
	})
})
