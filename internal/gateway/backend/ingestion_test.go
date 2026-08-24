package backend

import (
	"bytes"
	"context"
	"io"
	"testing"

	connect "connectrpc.com/connect"

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

func TestPassthroughStoresInlineContent(t *testing.T) {
	st := &memStorage{}
	b := NewPassthroughIngestion(st)
	resp, err := b.ConvertDocument(context.Background(), connect.NewRequest(&pb.ConvertDocumentRequest{
		Source: &pb.ConvertDocumentRequest_Content{Content: []byte("hello")},
	}))
	if err != nil {
		t.Fatalf("ConvertDocument: %v", err)
	}
	if resp.Msg.GetDocumentId() == "" {
		t.Fatal("empty document id")
	}
	if got, ok := st.puts[resp.Msg.GetDocumentId()]; !ok || string(got) != "hello" {
		t.Fatalf("stored content: got %q ok=%v, want %q", got, ok, "hello")
	}
}

func TestPassthroughRejectsSourceURI(t *testing.T) {
	b := NewPassthroughIngestion(&memStorage{})
	_, err := b.ConvertDocument(context.Background(), connect.NewRequest(&pb.ConvertDocumentRequest{
		Source: &pb.ConvertDocumentRequest_SourceUri{SourceUri: "file:///x"},
	}))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("code: got %v, want Unimplemented", connect.CodeOf(err))
	}
}
