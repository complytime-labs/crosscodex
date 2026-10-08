package natsbus_test

import (
	"context"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/natsbus"
)

var _ = Describe("AuditStreamNames", func() {
	It("lists the three audit streams in a stable order", func() {
		Expect(natsbus.AuditStreamNames()).To(Equal([]string{"AUDIT_LLM", "AUDIT_DECISIONS", "AUDIT_EVENTS"}))
	})
})

var _ = Describe("DialExternal", func() {
	It("refuses embedded mode without starting a server", func() {
		nc, err := natsbus.DialExternal(config.NATSConfig{URL: ""})
		Expect(nc).To(BeNil())
		Expect(err).To(MatchError(natsbus.ErrExternalURLRequired))
		Expect(err.Error()).To(ContainSubstring("nats.url"))
	})

	It("connects to an external server without creating any stream", func() {
		srv, err := natsserver.NewServer(&natsserver.Options{
			Host: "127.0.0.1", Port: -1, JetStream: true,
			StoreDir: GinkgoT().TempDir(), NoLog: true, NoSigs: true,
		})
		Expect(err).NotTo(HaveOccurred())
		go srv.Start()
		DeferCleanup(srv.Shutdown)
		Expect(srv.ReadyForConnections(10 * time.Second)).To(BeTrue())

		nc, err := natsbus.DialExternal(config.NATSConfig{URL: srv.ClientURL()})
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(nc.Close)

		js, err := jetstream.New(nc)
		Expect(err).NotTo(HaveOccurred())
		names := js.StreamNames(context.Background())
		var got []string
		for n := range names.Name() {
			got = append(got, n)
		}
		Expect(names.Err()).NotTo(HaveOccurred())
		Expect(got).To(BeEmpty())
	})
})
