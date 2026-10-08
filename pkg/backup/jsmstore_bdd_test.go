package backup_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/backup"
)

var _ = Describe("jsm.go stream store", Ordered, func() {
	ctx := context.Background()
	var (
		nc    *nats.Conn
		js    jetstream.JetStream
		store backup.StreamStore
		state backup.StreamState
		dir   string
	)

	BeforeAll(func() {
		srv, err := natsserver.NewServer(&natsserver.Options{
			Host: "127.0.0.1", Port: -1, JetStream: true,
			StoreDir: GinkgoT().TempDir(), NoLog: true, NoSigs: true,
		})
		Expect(err).NotTo(HaveOccurred())
		go srv.Start()
		DeferCleanup(srv.Shutdown)
		Expect(srv.ReadyForConnections(10 * time.Second)).To(BeTrue())

		nc, err = nats.Connect(srv.ClientURL())
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(nc.Close)
		js, err = jetstream.New(nc)
		Expect(err).NotTo(HaveOccurred())
		_, err = js.CreateStream(ctx, jetstream.StreamConfig{
			Name: "AUDIT_EVENTS", Subjects: []string{"crosscodex.audit.*.events.>"}, Storage: jetstream.FileStorage,
			MaxAge: 24 * time.Hour,
		})
		Expect(err).NotTo(HaveOccurred())
		for i := range 5 {
			_, err := js.Publish(ctx, "crosscodex.audit.acme-corp.events.x", []byte(fmt.Sprintf("m%d", i)))
			Expect(err).NotTo(HaveOccurred())
		}
		store, err = backup.NewJSMStreamStore(nc)
		Expect(err).NotTo(HaveOccurred())
		dir = GinkgoT().TempDir()
	})

	It("reports which streams exist", func() {
		Expect(store.Exists(ctx, "AUDIT_EVENTS")).To(BeTrue())
		Expect(store.Exists(ctx, "AUDIT_LLM")).To(BeFalse())
	})

	It("snapshots the stream's files and state", func() {
		var err error
		state, err = store.Snapshot(ctx, "AUDIT_EVENTS", dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(state).To(Equal(backup.StreamState{FirstSeq: 1, LastSeq: 5, Msgs: 5}))
		Expect(filepath.Join(dir, "backup.json")).To(BeAnExistingFile())
		entries, err := os.ReadDir(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(HaveLen(2), "backup.json plus one data archive")
	})

	It("restores a deleted stream with the same messages and sequences", func() {
		Expect(js.DeleteStream(ctx, "AUDIT_EVENTS")).To(Succeed())
		restored, err := store.Restore(ctx, "AUDIT_EVENTS", dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(restored).To(Equal(state))
		s, err := js.Stream(ctx, "AUDIT_EVENTS")
		Expect(err).NotTo(HaveOccurred())
		msg, err := s.GetMsg(ctx, 3)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(msg.Data)).To(Equal("m2"))

		info, err := s.Info(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Config.Subjects).To(Equal([]string{"crosscodex.audit.*.events.>"}))
		Expect(info.Config.MaxAge).To(Equal(24 * time.Hour))
	})
})
