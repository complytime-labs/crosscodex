// Package backend holds reusable gateway backend implementations shared by
// the embedded CLI and the crosscodexd daemon.
package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	connect "connectrpc.com/connect"
	"github.com/google/uuid"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/storage"
)

// PassthroughIngestion stores raw inline document content to object storage.
// It performs no format conversion; source_uri ingestion is not implemented.
type PassthroughIngestion struct {
	storage storage.Provider
}

// NewPassthroughIngestion returns an ingestion backend that writes inline
// content to storage under documents/<uuid>.json.
func NewPassthroughIngestion(s storage.Provider) *PassthroughIngestion {
	return &PassthroughIngestion{storage: s}
}

func (b *PassthroughIngestion) ConvertDocument(ctx context.Context, req *connect.Request[pb.ConvertDocumentRequest]) (*connect.Response[pb.ConvertDocumentResponse], error) {
	src, ok := req.Msg.Source.(*pb.ConvertDocumentRequest_Content)
	if !ok {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("only inline content is supported; source_uri is not implemented"))
	}
	content := src.Content

	key := fmt.Sprintf("documents/%s.json", uuid.New().String())
	if err := b.storage.Put(ctx, key, bytes.NewReader(content)); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("store document: %w", err))
	}

	return connect.NewResponse(&pb.ConvertDocumentResponse{
		DocumentId: key,
		Status:     pb.JobStatus_JOB_STATUS_COMPLETED,
	}), nil
}
