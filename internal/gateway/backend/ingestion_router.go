package backend

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	connect "connectrpc.com/connect"
	"github.com/google/uuid"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/tlsconfig"
)

// ingestionTLSTarget is the tls.targets entry the ingestion client dials with.
const ingestionTLSTarget = "ingestion"

// IngestionRouter picks the ingestion backend for each document by its
// content. OSCAL JSON of any model (oscal.IsModelJSON, a superset of the
// catalog sniff ParseCatalog uses) is stored unchanged by the passthrough: a
// catalog needs no conversion, and ParseCatalog explains why another model
// (a profile, say) cannot be imported. Any other inline content goes to the ingestion
// client when one is configured. Without a client, and for source_uri
// requests, every request goes to the passthrough, so behaviour is what it
// was before the client existed: non-OSCAL bytes are stored and ParseCatalog
// rejects them with its unsupported-format error, and source_uri is
// Unimplemented.
//
// It implements gateway.IngestionBackend. It takes the concrete backends
// rather than that interface because internal/gateway/... may not import
// internal/gateway (depguard).
type IngestionRouter struct {
	passthrough *PassthroughIngestion
	client      *IngestionClient // nil: ingestion is not configured
}

// NewIngestionRouter returns the gateway's ingestion backend over passthrough
// and client. A nil client means the ingestion service is not configured.
func NewIngestionRouter(passthrough *PassthroughIngestion, client *IngestionClient) *IngestionRouter {
	return &IngestionRouter{passthrough: passthrough, client: client}
}

func (r *IngestionRouter) ConvertDocument(ctx context.Context, req *connect.Request[pb.ConvertDocumentRequest]) (*connect.Response[pb.ConvertDocumentResponse], error) {
	src, ok := req.Msg.GetSource().(*pb.ConvertDocumentRequest_Content)
	if !ok || r.client == nil || oscal.IsModelJSON(src.Content) {
		return r.passthrough.ConvertDocument(ctx, req)
	}
	return r.client.ConvertDocument(ctx, req)
}

// DiscardDocument deletes documentID through the backend that stored it,
// told apart by the key's layout: the passthrough stores documents/<uuid>.json
// and the ingestion service writes documents/<id>/<file>. Any other key, or
// an ingestion key while no client is configured, is refused and nothing is
// deleted, so a malformed ID can never delete through the wrong backend.
func (r *IngestionRouter) DiscardDocument(ctx context.Context, documentID string) error {
	switch {
	case isPassthroughKey(documentID):
		return r.passthrough.DiscardDocument(ctx, documentID)
	case r.client != nil && isIngestionOutputKey(documentID):
		return r.client.DiscardDocument(ctx, documentID)
	default:
		return fmt.Errorf("cannot discard document %q: it is neither a passthrough key (documents/<uuid>.json) "+
			"nor, with ingestion.address set, an ingestion output key (documents/<id>/<file>), so nothing was deleted; "+
			"discard only a document_id that ConvertDocument returned", documentID)
	}
}

// isPassthroughKey reports whether key is documents/<uuid>.json with the
// UUID in the canonical form PassthroughIngestion writes.
func isPassthroughKey(key string) bool {
	name, ok := strings.CutPrefix(key, "documents/")
	if !ok {
		return false
	}
	id, ok := strings.CutSuffix(name, ".json")
	if !ok {
		return false
	}
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.String() == id
}

// isIngestionOutputKey reports whether key is documents/<id>/<file>, the
// tenant-relative layout ResolveDocumentKey returns for ingestion output.
func isIngestionOutputKey(key string) bool {
	segments := strings.Split(key, "/")
	if len(segments) != 3 || segments[0] != "documents" {
		return false
	}
	for _, seg := range segments[1:] {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// NewIngestion returns the ingestion backend the gateway serves with, shared
// by the crosscodexd daemon and the embedded CLI daemon: a router over a
// PassthroughIngestion on store and, when ing.Address is set, an
// IngestionClient.
//
// With ing.Address empty nothing else is read, so tlsCfg and objects may be
// anything. Otherwise:
//   - the client speaks gRPC to ing.Address over the TLS that
//     tls.targets.ingestion resolves to (pkg/tlsconfig): HTTP/2 over TLS, or
//     unencrypted HTTP/2 when the resolved mode is off;
//   - objects describes the object storage the ingestion service shares with
//     store. For the local backend, base_path must exist: the file:// URIs
//     ingestion returns hold its absolute, symlink-resolved path, so
//     base_path is resolved the same way (filepath.Abs, then
//     filepath.EvalSymlinks, as the local storage provider does) before URIs
//     are checked against it. The s3 backend needs bucket.
//
// Every refusal names the setting to fix, so callers can fail startup with it.
func NewIngestion(ctx context.Context, ing config.IngestionConfig, tlsCfg config.TLSConfig, objects config.ObjectStorageConfig, store storage.Provider, opts ...IngestionClientOption) (*IngestionRouter, error) {
	passthrough := NewPassthroughIngestion(store)
	if ing.Address == "" {
		return NewIngestionRouter(passthrough, nil), nil
	}

	var sb StorageBackend
	switch storage.ProviderType(objects.Backend) {
	case storage.ProviderTypeLocal:
		if objects.BasePath == "" {
			return nil, fmt.Errorf("ingestion.address is set but storage.objects.base_path is empty: " +
				"set it to the directory the ingestion service writes its output to")
		}
		abs, err := filepath.Abs(objects.BasePath)
		if err != nil {
			return nil, fmt.Errorf("resolve storage.objects.base_path %q: %w", objects.BasePath, err)
		}
		root, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, fmt.Errorf("resolve storage.objects.base_path %q: %w; with ingestion.address set it must be an "+
				"existing directory shared with the ingestion service, so create it or fix the path", objects.BasePath, err)
		}
		sb = StorageBackend{Kind: string(storage.ProviderTypeLocal), Root: root}
	case storage.ProviderTypeS3:
		if objects.Bucket == "" {
			return nil, fmt.Errorf("ingestion.address is set but storage.objects.bucket is empty: " +
				"set it to the bucket the ingestion service writes its output to")
		}
		sb = StorageBackend{Kind: string(storage.ProviderTypeS3), Bucket: objects.Bucket}
	default:
		return nil, fmt.Errorf(`storage.objects.backend %q is not supported with ingestion.address set: set it to "local" or "s3"`, objects.Backend)
	}

	tc, err := tlsconfig.BuildTLSConfig(ctx, tlsCfg, ingestionTLSTarget)
	if err != nil {
		return nil, fmt.Errorf("resolve tls.targets.%s for ingestion.address %s: %w", ingestionTLSTarget, ing.Address, err)
	}
	// gRPC needs HTTP/2 whichever way it is carried.
	protocols := new(http.Protocols)
	scheme := "https://"
	if tc == nil {
		protocols.SetUnencryptedHTTP2(true)
		scheme = "http://"
	} else {
		protocols.SetHTTP2(true)
	}
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: tc, Protocols: protocols}}
	rpc := crosscodexv1connect.NewIngestionServiceClient(httpClient, scheme+ing.Address, connect.WithGRPC())

	client, err := NewIngestionClient(rpc, store, sb, ing, opts...)
	if err != nil {
		return nil, err
	}
	return NewIngestionRouter(passthrough, client), nil
}
