package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
)

func (s *Service) StreamDocument(ctx context.Context, stream *connect.ClientStream[pb.StreamDocumentChunk]) (*connect.Response[pb.SubmitDocumentResponse], error) {
	start := time.Now()
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}

	ctx, endSpan := s.startHandlerSpan(ctx, "StreamDocument", identity)
	defer endSpan()

	var meta *pb.StreamDocumentMetadata
	var buf bytes.Buffer

	for stream.Receive() {
		chunk := stream.Msg()
		switch p := chunk.Payload.(type) {
		case *pb.StreamDocumentChunk_Metadata:
			if meta != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("metadata must appear exactly once as the first message"))
			}
			meta = p.Metadata
		case *pb.StreamDocumentChunk_Chunk:
			if meta == nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("first message must contain metadata"))
			}
			buf.Write(p.Chunk)
			if buf.Len() > s.maxUploadSize {
				return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("upload exceeds maximum size of %d bytes", s.maxUploadSize))
			}
		}
	}
	if err := stream.Err(); err != nil {
		s.recordMetrics(ctx, "StreamDocument", start, connect.CodeOf(err))
		return nil, connect.NewError(connect.CodeUnknown, fmt.Errorf("stream error: %w", err))
	}

	if meta == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("first message must contain metadata"))
	}

	return s.handleStreamedDocument(ctx, start, meta, buf.Bytes())
}

func (s *Service) handleStreamedDocument(ctx context.Context, start time.Time, meta *pb.StreamDocumentMetadata, content []byte) (*connect.Response[pb.SubmitDocumentResponse], error) {
	// Identity already validated by StreamDocument caller; re-check here for defensive isolation (tests call directly)
	identity := identityFromContext(ctx)
	if identity == nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}

	if meta == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("metadata is required"))
	}

	return s.processDocument(ctx, start, processDocumentInput{
		convertReq: &pb.ConvertDocumentRequest{
			Source:   &pb.ConvertDocumentRequest_Content{Content: content},
			Metadata: meta.GetContentMetadata(),
		},
		catalogFormat:   meta.GetCatalogFormat(),
		catalogName:     meta.GetCatalogName(),
		targetCatalogID: meta.GetTargetCatalogId(),
		synthesisConfig: meta.GetSynthesisConfig(),
		rpcName:         "StreamDocument",
	})
}
