package backend

import (
	"context"

	connect "connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
)

var _ = Describe("PassthroughIngestion", func() {
	It("stores inline content", func() {
		st := &storagetest.Fake{}
		b := NewPassthroughIngestion(st)
		resp, err := b.ConvertDocument(context.Background(), connect.NewRequest(&pb.ConvertDocumentRequest{
			Source: &pb.ConvertDocumentRequest_Content{Content: []byte("hello")},
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.GetDocumentId()).NotTo(BeEmpty())

		got, ok := st.Objects()[resp.Msg.GetDocumentId()]
		Expect(ok).To(BeTrue(), "stored content: want key %q present", resp.Msg.GetDocumentId())
		Expect(string(got)).To(Equal("hello"))
	})

	It("rejects a source URI", func() {
		b := NewPassthroughIngestion(&storagetest.Fake{})
		_, err := b.ConvertDocument(context.Background(), connect.NewRequest(&pb.ConvertDocumentRequest{
			Source: &pb.ConvertDocumentRequest_SourceUri{SourceUri: "file:///x"},
		}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnimplemented))
	})

	It("discards a stored document so a rejected import leaves no blob", func() {
		st, err := storage.NewLocal(GinkgoT().TempDir(), "test")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(st.Close)
		b := NewPassthroughIngestion(st)
		ctx := context.Background()
		resp, err := b.ConvertDocument(ctx, connect.NewRequest(&pb.ConvertDocumentRequest{
			Source: &pb.ConvertDocumentRequest_Content{Content: []byte("hello")},
		}))
		Expect(err).NotTo(HaveOccurred())

		Expect(b.DiscardDocument(ctx, resp.Msg.GetDocumentId())).To(Succeed())

		exists, err := st.Exists(ctx, resp.Msg.GetDocumentId())
		Expect(err).NotTo(HaveOccurred())
		Expect(exists).To(BeFalse())
	})

	It("reports a storage failure when discarding", func() {
		st, err := storage.NewLocal(GinkgoT().TempDir(), "test")
		Expect(err).NotTo(HaveOccurred())
		Expect(st.Close()).To(Succeed())

		err = NewPassthroughIngestion(st).DiscardDocument(context.Background(), "documents/x.json")
		Expect(err).To(MatchError(ContainSubstring("discard document documents/x.json")))
	})
})
