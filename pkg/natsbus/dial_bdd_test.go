package natsbus_test

import (
	"context"
	"os"
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

// startExternalServer runs a JetStream-enabled nats-server on a random port,
// standing in for the daemon's server that an admin one-shot attaches to.
func startExternalServer() *natsserver.Server {
	srv, err := natsserver.NewServer(&natsserver.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true,
		StoreDir: GinkgoT().TempDir(), NoLog: true, NoSigs: true,
	})
	Expect(err).NotTo(HaveOccurred())
	go srv.Start()
	DeferCleanup(srv.Shutdown)
	Expect(srv.ReadyForConnections(10 * time.Second)).To(BeTrue())
	return srv
}

func streamNames(url string) []string {
	nc, err := natsbus.DialExternal(config.NATSConfig{URL: url})
	Expect(err).NotTo(HaveOccurred())
	defer nc.Close()
	js, err := jetstream.New(nc)
	Expect(err).NotTo(HaveOccurred())
	names := js.StreamNames(context.Background())
	var got []string
	for n := range names.Name() {
		got = append(got, n)
	}
	Expect(names.Err()).NotTo(HaveOccurred())
	return got
}

var _ = Describe("New with WithExistingStreams", func() {
	It("refuses embedded mode without starting a server on the store directory", func() {
		storeDir := GinkgoT().TempDir()
		cfg := config.NATSConfig{URL: ""}
		cfg.Embedded.StoreDir = storeDir

		c, err := natsbus.New(cfg, natsbus.WithExistingStreams())

		Expect(c).To(BeNil())
		Expect(err).To(MatchError(natsbus.ErrExternalURLRequired))
		Expect(err.Error()).To(ContainSubstring("Set nats.url to the daemon's NATS server"))
		Expect(os.ReadDir(storeDir)).To(BeEmpty(), "no embedded server may touch the store directory")
	})

	It("refuses a server without the audit streams and creates none", func() {
		srv := startExternalServer()

		c, err := natsbus.New(config.NATSConfig{URL: srv.ClientURL()}, natsbus.WithExistingStreams())

		Expect(c).To(BeNil())
		Expect(err).To(MatchError(natsbus.ErrStreamNotFound))
		Expect(err.Error()).To(ContainSubstring("AUDIT_LLM"))
		Expect(err.Error()).To(ContainSubstring("Start the daemon"))
		Expect(streamNames(srv.ClientURL())).To(BeEmpty())
	})

	It("attaches to existing audit streams without changing their retention", func() {
		srv := startExternalServer()
		daemonCfg := config.NATSConfig{URL: srv.ClientURL()}
		daemonCfg.Streams.AuditLLMRetention = 90 * 24 * time.Hour
		daemonCfg.Streams.AuditEventsRetention = 30 * 24 * time.Hour
		daemon, err := natsbus.New(daemonCfg)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(daemon.Close)
		before, err := daemon.AuditStreamRetention(context.Background())
		Expect(err).NotTo(HaveOccurred())

		adminCfg := config.NATSConfig{URL: srv.ClientURL()}
		adminCfg.Streams.AuditLLMRetention = time.Hour
		adminCfg.Streams.AuditEventsRetention = time.Hour
		admin, err := natsbus.New(adminCfg, natsbus.WithExistingStreams())
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(admin.Close)

		after, err := daemon.AuditStreamRetention(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(after).To(Equal(before))
		Expect(after["AUDIT_LLM"]).To(Equal(90 * 24 * time.Hour))
	})
})
