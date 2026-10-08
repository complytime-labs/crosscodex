package backup_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/backup"
	"github.com/complytime-labs/crosscodex/pkg/storage"
)

func drawHex(t *rapid.T, n int, label string) string {
	return hex.EncodeToString(rapid.SliceOfN(rapid.Byte(), n, n).Draw(t, label))
}

func drawStats(t *rapid.T, start time.Time, label string) backup.CaptureStats {
	s := start.Add(time.Duration(rapid.Int64Range(0, 1e12).Draw(t, label+".offset")))
	e := s.Add(time.Duration(rapid.Int64Range(0, 1e12).Draw(t, label+".duration")))
	return backup.CaptureStats{
		Window:                backup.Window{Start: s, End: e},
		Bytes:                 rapid.Int64Range(0, 1<<40).Draw(t, label+".bytes"),
		ThroughputBytesPerSec: float64(rapid.Int64Range(0, 1<<30).Draw(t, label+".throughput")),
	}
}

// drawManifest draws an arbitrary valid manifest.
func drawManifest(t *rapid.T) *backup.Manifest {
	start := time.Unix(rapid.Int64Range(1e9, 4e9).Draw(t, "start"), rapid.Int64Range(0, 999_999_999).Draw(t, "ns")).UTC()
	id, err := backup.NewPointID(start, bytes.NewReader(rapid.SliceOfN(rapid.Byte(), 4, 4).Draw(t, "rand")))
	if err != nil {
		t.Fatalf("NewPointID: %v", err)
	}
	segment := rapid.Uint64Range(1, 1<<40).Draw(t, "segment")
	tenants := map[string][]backup.ObjectEntry{}
	for _, tid := range rapid.SliceOfNDistinct(rapid.StringMatching(`[a-z][a-z0-9-]{1,10}[a-z0-9]`), 0, 4, rapid.ID[string]).Draw(t, "tenants") {
		keys := rapid.SliceOfNDistinct(rapid.StringMatching(`[a-z0-9]{1,8}(/[a-z0-9]{1,8}){0,3}\.json`), 0, 5, rapid.ID[string]).Draw(t, "keys")
		entries := make([]backup.ObjectEntry, 0, len(keys))
		for _, k := range keys {
			entries = append(entries, backup.ObjectEntry{Key: k, SHA256: drawHex(t, 32, "sha"), Size: rapid.Int64Range(0, 1<<30).Draw(t, "size")})
		}
		tenants[tid] = entries
	}
	var streams []backup.StreamCapture
	for _, name := range rapid.SliceOfNDistinct(rapid.SampledFrom([]string{"AUDIT_LLM", "AUDIT_DECISIONS", "AUDIT_EVENTS"}), 0, 3, rapid.ID[string]).Draw(t, "streams") {
		first := rapid.Uint64Range(1, 1<<40).Draw(t, "first")
		span := rapid.Uint64Range(0, 1<<20).Draw(t, "span")
		streams = append(streams, backup.StreamCapture{
			Stream: name,
			Files: []backup.StreamFile{
				{Name: "backup.json", SHA256: drawHex(t, 32, "meta"), Size: 100},
				{Name: rapid.SampledFrom([]string{"stream.arc.s2", "stream.tar.s2"}).Draw(t, "data"), SHA256: drawHex(t, 32, "data.sha"), Size: 1000},
			},
			State: backup.StreamState{FirstSeq: first, LastSeq: first + span, Msgs: rapid.Uint64Range(1, span+1).Draw(t, "msgs")},
		})
	}
	return &backup.Manifest{
		SchemaVersion: backup.SchemaVersion,
		ID:            id,
		StartedAt:     start,
		FinishedAt:    start.Add(time.Duration(rapid.Int64Range(0, 1e13).Draw(t, "elapsed"))),
		Postgres: backup.PostgresCapture{
			BackupName:   fmt.Sprintf("base_%08X%016X", rapid.Uint32Range(1, 1<<31).Draw(t, "tl"), segment),
			StartLSN:     segment << 24,
			FinishLSN:    segment<<24 + rapid.Uint64Range(0, 1<<24).Draw(t, "lsn"),
			Timeline:     rapid.Uint32Range(1, 1<<31).Draw(t, "timeline"),
			CaptureStats: drawStats(t, start, "postgres"),
		},
		Objects: backup.ObjectsCapture{Tenants: tenants, CaptureStats: drawStats(t, start, "objects")},
		NATS:    backup.NATSCapture{Streams: streams, CaptureStats: drawStats(t, start, "nats")},
	}
}

var _ = Describe("Property Specifications", Ordered, func() {
	It("manifest encode/decode round-trips byte for byte", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			var first bytes.Buffer
			if err := backup.Encode(&first, drawManifest(t)); err != nil {
				t.Fatalf("Encode rejected a valid manifest: %v", err)
			}
			decoded, err := backup.Decode(bytes.NewReader(first.Bytes()))
			if err != nil {
				t.Fatalf("Decode rejected an encoded manifest: %v", err)
			}
			var second bytes.Buffer
			if err := backup.Encode(&second, decoded); err != nil {
				t.Fatalf("re-encode: %v", err)
			}
			if !bytes.Equal(first.Bytes(), second.Bytes()) {
				t.Fatalf("round trip changed the manifest:\n%s\n---\n%s", first.String(), second.String())
			}
		})
	})

	It("point IDs always validate and sort by time", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			a := time.Unix(rapid.Int64Range(0, 4e9).Draw(t, "a"), 0)
			b := a.Add(time.Duration(rapid.Int64Range(1, 1e6).Draw(t, "gap")) * time.Second)
			ida, _ := backup.NewPointID(a, bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff}))
			idb, _ := backup.NewPointID(b, bytes.NewReader([]byte{0, 0, 0, 0}))
			if backup.ValidatePointID(ida) != nil || backup.ValidatePointID(idb) != nil || (ida >= idb) {
				t.Fatalf("ids %q, %q: invalid or out of order", ida, idb)
			}
		})
	})

	It("stores one blob per distinct content, however often it repeats", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			contents := rapid.SliceOfN(rapid.SampledFrom([]string{"a", "b", "c", "d"}), 1, 12).Draw(t, "contents")
			root, err := os.MkdirTemp(GinkgoT().TempDir(), "prop-*")
			if err != nil {
				t.Fatal(err)
			}
			srcStore, err := storage.NewLocal(filepath.Join(root, "src"), "acme-corp")
			if err != nil {
				t.Fatal(err)
			}
			distinct := map[string]bool{}
			for i, c := range contents {
				if err := srcStore.Put(context.Background(), fmt.Sprintf("k%d.json", i), strings.NewReader(c)); err != nil {
					t.Fatal(err)
				}
				distinct[c] = true
			}
			repoStore, err := storage.NewLocal(filepath.Join(root, "repo"), backup.Namespace)
			if err != nil {
				t.Fatal(err)
			}
			repo := backup.NewRepository(repoStore)
			open := func(tid string) (storage.Provider, error) { return storage.NewLocal(filepath.Join(root, "src"), tid) }
			if _, _, err := backup.SnapshotObjects(context.Background(), repo, []string{"acme-corp"}, open, root); err != nil {
				t.Fatal(err)
			}
			blobs, err := os.ReadDir(filepath.Join(root, "repo", "backup", "objects", "blobs"))
			if err != nil {
				t.Fatal(err)
			}
			if len(blobs) != len(distinct) {
				t.Fatalf("%d blobs for %d distinct contents", len(blobs), len(distinct))
			}
		})
	})
})
