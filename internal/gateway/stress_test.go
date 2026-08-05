//go:build stress

package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"go.uber.org/goleak"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
)

// hashIngestion echoes a content-derived DocumentId so a concurrent caller can
// prove the server buffered exactly its own bytes (no cross-stream corruption).
type hashIngestion struct{}

func (hashIngestion) ConvertDocument(_ context.Context, req *connect.Request[pb.ConvertDocumentRequest]) (*connect.Response[pb.ConvertDocumentResponse], error) {
	return connect.NewResponse(&pb.ConvertDocumentResponse{
		DocumentId: sha256hex(req.Msg.GetContent()),
	}), nil
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// synthPayload returns deterministic bytes of the requested size with a marker
// prefix so different logical uploads have different content (and hashes).
func synthPayload(size int, marker string) []byte {
	out := make([]byte, size)
	copy(out, marker)
	for i := len(marker); i < size; i++ {
		out[i] = byte('a' + (i % 26))
	}
	return out
}

func stressEnv(t *testing.T, ing IngestionBackend) *streamTestEnv {
	t.Helper()
	if ing == nil {
		ing = hashIngestion{}
	}
	return newStreamTestEnv(t, defaultIdentity(),
		WithIngestionBackend(ing),
		WithCatalogBackend(&stubCatalog{}),
		WithPipelineBackend(&stubPipeline{}),
	)
}

func uploadStream(ctx context.Context, c crosscodexv1connect.GatewayServiceClient, name string, payload []byte, chunk int) (*pb.SubmitDocumentResponse, error) {
	stream := c.StreamDocument(ctx)
	if err := stream.Send(&pb.StreamDocumentChunk{
		Payload: &pb.StreamDocumentChunk_Metadata{
			Metadata: &pb.StreamDocumentMetadata{
				CatalogFormat: pb.CatalogFormat_CATALOG_FORMAT_OSCAL,
				CatalogName:   name,
			},
		},
	}); err != nil {
		return nil, err
	}
	for off := 0; off < len(payload); off += chunk {
		end := off + chunk
		if end > len(payload) {
			end = len(payload)
		}
		if err := stream.Send(&pb.StreamDocumentChunk{
			Payload: &pb.StreamDocumentChunk_Chunk{Chunk: payload[off:end]},
		}); err != nil {
			return nil, err
		}
	}
	resp, err := stream.CloseAndReceive()
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func TestStress_ParallelLargeImports_NoCorruption(t *testing.T) {
	// hashIngestion is used via stressEnv; import gateway pkg symbols are in-package.
	env := stressEnv(t, nil)
	defer env.close()

	const (
		workers   = 16
		size      = 10 << 20 // 10 MB
		chunkSize = 1 << 20  // 1 MB
	)

	type result struct {
		want, got string
		err       error
	}
	results := make([]result, workers)
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload := synthPayload(size, fmt.Sprintf("worker-%02d-", i))
			resp, err := uploadStream(ctx, env.client, fmt.Sprintf("cat-%02d", i), payload, chunkSize)
			results[i] = result{want: sha256hex(payload), err: err}
			if resp != nil {
				results[i].got = resp.GetDocumentId()
			}
		}(i)
	}
	wg.Wait()

	for i, r := range results {
		if r.err != nil {
			t.Errorf("worker %d: upload failed: %v", i, r.err)
			continue
		}
		if r.got != r.want {
			t.Errorf("worker %d: DocumentId mismatch — got %s want %s (data corruption / cross-stream mixup)", i, r.got, r.want)
		}
	}
}

func TestStress_MixedSizes_NoStarvation(t *testing.T) {
	env := stressEnv(t, nil)
	defer env.close()

	sizes := []int{100 << 10, 1 << 20, 10 << 20, 50 << 20}
	const chunkSize = 1 << 20
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errs := make([]error, len(sizes))
	durs := make([]time.Duration, len(sizes))
	for i, sz := range sizes {
		wg.Add(1)
		go func(i, sz int) {
			defer wg.Done()
			payload := synthPayload(sz, fmt.Sprintf("size-%d-", sz))
			t0 := time.Now()
			_, err := uploadStream(ctx, env.client, fmt.Sprintf("mixed-%d", i), payload, chunkSize)
			durs[i] = time.Since(t0)
			errs[i] = err
		}(i, sz)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("size index %d (%d bytes) failed: %v", i, sizes[i], err)
		}
	}
	// Smallest upload must not be dominated by the largest — it should finish
	// no slower than the largest upload. Allow a jitter margin so a
	// sub-scheduler-quantum inversion on a loaded runner is not mistaken for
	// starvation; a genuine stall makes the small upload wait out the 50MB
	// transfer, well beyond this tolerance.
	const jitter = 250 * time.Millisecond
	if errs[0] == nil && durs[0] > durs[len(durs)-1]+jitter {
		t.Errorf("small upload (%v) slower than largest (%v) beyond jitter — possible starvation", durs[0], durs[len(durs)-1])
	}
}

func TestStress_ConnectionChurn(t *testing.T) {
	env := stressEnv(t, nil)
	defer env.close()

	const cycles = 200
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	payload := synthPayload(64<<10, "churn-")
	for i := 0; i < cycles; i++ {
		if _, err := uploadStream(ctx, env.client, "churn", payload, 32<<10); err != nil {
			t.Fatalf("cycle %d failed: %v", i, err)
		}
	}
}

func TestStress_OversizeRejectedConcurrently(t *testing.T) {
	const limit = 2 << 20 // 2 MB
	env := newStreamTestEnv(t, defaultIdentity(),
		WithIngestionBackend(hashIngestion{}),
		WithCatalogBackend(&stubCatalog{}),
		WithPipelineBackend(&stubPipeline{}),
		WithMaxUploadSize(limit),
	)
	defer env.close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	const workers = 8
	codes := make([]connect.Code, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload := synthPayload(limit+(1<<20), fmt.Sprintf("big-%d-", i))
			_, err := uploadStream(ctx, env.client, "oversize", payload, 1<<20)
			codes[i] = connect.CodeOf(err)
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != connect.CodeResourceExhausted {
			t.Errorf("worker %d: got %v, want ResourceExhausted", i, c)
		}
	}
}

type slowIngestion struct{}

func (slowIngestion) ConvertDocument(ctx context.Context, _ *connect.Request[pb.ConvertDocumentRequest]) (*connect.Response[pb.ConvertDocumentResponse], error) {
	<-ctx.Done()
	return nil, connect.NewError(connect.CodeDeadlineExceeded, ctx.Err())
}

func TestStress_DeadlineExceeded(t *testing.T) {
	env := stressEnv(t, slowIngestion{})
	defer env.close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := uploadStream(ctx, env.client, "slow", synthPayload(1<<20, "slow-"), 256<<10)
	if got := connect.CodeOf(err); got != connect.CodeDeadlineExceeded {
		t.Fatalf("got %v, want DeadlineExceeded (err=%v)", got, err)
	}
}

type failOnMarkerIngestion struct{ marker string }

func (f failOnMarkerIngestion) ConvertDocument(_ context.Context, req *connect.Request[pb.ConvertDocumentRequest]) (*connect.Response[pb.ConvertDocumentResponse], error) {
	content := req.Msg.GetContent()
	if len(content) >= len(f.marker) && string(content[:len(f.marker)]) == f.marker {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("simulated malformed document"))
	}
	return connect.NewResponse(&pb.ConvertDocumentResponse{DocumentId: sha256hex(content)}), nil
}

func TestStress_PartialFailureIsolation(t *testing.T) {
	const badMarker = "POISON-"
	env := stressEnv(t, failOnMarkerIngestion{marker: badMarker})
	defer env.close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const workers = 8
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			marker := fmt.Sprintf("ok-%d-", i)
			if i == 3 {
				marker = badMarker
			}
			payload := synthPayload(1<<20, marker)
			_, errs[i] = uploadStream(ctx, env.client, "batch", payload, 256<<10)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if i == 3 {
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("poison worker: got %v, want InvalidArgument", connect.CodeOf(err))
			}
			continue
		}
		if err != nil {
			t.Errorf("sibling worker %d affected by peer failure: %v", i, err)
		}
	}
}

func TestStress_NoGoroutineLeak(t *testing.T) {
	env := stressEnv(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Warm up one upload so the HTTP transport's persistent-conn goroutines
	// exist before we snapshot the baseline to ignore.
	if _, err := uploadStream(ctx, env.client, "warmup", synthPayload(1<<20, "warm-"), 256<<10); err != nil {
		env.close()
		t.Fatalf("warmup failed: %v", err)
	}
	// Give transport goroutines a moment to settle.
	time.Sleep(100 * time.Millisecond)
	ignore := goleak.IgnoreCurrent()

	const workers = 16
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = uploadStream(ctx, env.client, "leak", synthPayload(4<<20, fmt.Sprintf("leak-%d-", i)), 1<<20)
		}(i)
	}
	wg.Wait()

	// Allow HTTP transport to gracefully wind down persistent connections,
	// then close the environment to shut down the test server.
	time.Sleep(100 * time.Millisecond)
	env.close()
	time.Sleep(100 * time.Millisecond)

	goleak.VerifyNone(t, ignore)
}

func BenchmarkStreamDocument10MB(b *testing.B) {
	env := newStreamTestEnv(&testing.T{}, defaultIdentity(),
		WithIngestionBackend(hashIngestion{}),
		WithCatalogBackend(&stubCatalog{}),
		WithPipelineBackend(&stubPipeline{}),
	)
	defer env.close()

	payload := synthPayload(10<<20, "bench-")
	ctx := context.Background()
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := uploadStream(ctx, env.client, "bench", payload, 1<<20); err != nil {
			b.Fatalf("upload failed: %v", err)
		}
	}
}
