package backend

import (
	"bytes"
	"context"
	"io"

	connect "connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/storage"
)

type memStorage struct{ puts map[string][]byte }

func (m *memStorage) Put(_ context.Context, key string, r io.Reader) error {
	if m.puts == nil {
		m.puts = map[string][]byte{}
	}
	b, err := io.ReadAll(r)
	m.puts[key] = b
	return err
}
func (m *memStorage) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(nil)), nil
}
func (m *memStorage) Delete(context.Context, string) error         { return nil }
func (m *memStorage) Exists(context.Context, string) (bool, error) { return true, nil }
func (m *memStorage) List(context.Context, string) ([]storage.ObjectMetadata, error) {
	return nil, nil
}
func (m *memStorage) Stat(context.Context, string) (*storage.ObjectMetadata, error) {
	return nil, nil
}
func (m *memStorage) Close() error { return nil }

var _ = Describe("PassthroughIngestion", func() {
	It("stores inline content", func() {
		st := &memStorage{}
		b := NewPassthroughIngestion(st)
		resp, err := b.ConvertDocument(context.Background(), connect.NewRequest(&pb.ConvertDocumentRequest{
			Source: &pb.ConvertDocumentRequest_Content{Content: []byte("hello")},
		}))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.Msg.GetDocumentId()).NotTo(BeEmpty())

		got, ok := st.puts[resp.Msg.GetDocumentId()]
		Expect(ok).To(BeTrue(), "stored content: want key %q present", resp.Msg.GetDocumentId())
		Expect(string(got)).To(Equal("hello"))
	})

	It("rejects a source URI", func() {
		b := NewPassthroughIngestion(&memStorage{})
		_, err := b.ConvertDocument(context.Background(), connect.NewRequest(&pb.ConvertDocumentRequest{
			Source: &pb.ConvertDocumentRequest_SourceUri{SourceUri: "file:///x"},
		}))
		Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnimplemented))
	})
})
