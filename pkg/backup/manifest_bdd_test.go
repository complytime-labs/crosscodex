package backup_test

import (
	"bytes"
	"math"
	"strings"
	"testing/iotest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/backup"
)

var sampleTime = time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC)

func sampleStats(offset time.Duration) backup.CaptureStats {
	return backup.CaptureStats{
		Window:                backup.Window{Start: sampleTime.Add(offset), End: sampleTime.Add(offset + 10*time.Second)},
		Bytes:                 1000,
		ThroughputBytesPerSec: 100,
	}
}

// sampleManifest returns a fresh valid manifest; specs mutate their copy.
func sampleManifest() *backup.Manifest {
	return &backup.Manifest{
		SchemaVersion: backup.SchemaVersion,
		ID:            "20261007T010000Z-0a1b2c3d",
		StartedAt:     sampleTime,
		FinishedAt:    sampleTime.Add(time.Minute),
		Postgres: backup.PostgresCapture{
			BackupName: "base_000000010000000000000004", StartLSN: 0x4000028, FinishLSN: 0x4000100,
			Timeline: 1, CaptureStats: sampleStats(0),
		},
		Objects: backup.ObjectsCapture{
			Tenants: map[string][]backup.ObjectEntry{
				"acme-corp": {{Key: "artifacts/a.json", SHA256: strings.Repeat("a", 64), Size: 10}},
				"globex":    {},
			},
			CaptureStats: sampleStats(10 * time.Second),
		},
		NATS: backup.NATSCapture{
			Streams: []backup.StreamCapture{{
				Stream: "AUDIT_EVENTS",
				Files: []backup.StreamFile{
					{Name: "backup.json", SHA256: strings.Repeat("b", 64), Size: 300},
					{Name: "stream.arc.s2", SHA256: strings.Repeat("c", 64), Size: 700},
				},
				State: backup.StreamState{FirstSeq: 1, LastSeq: 5, Msgs: 5},
			}},
			CaptureStats: sampleStats(20 * time.Second),
		},
	}
}

func encode(m *backup.Manifest) []byte {
	var buf bytes.Buffer
	Expect(backup.Encode(&buf, m)).To(Succeed())
	return buf.Bytes()
}

var _ = Describe("Point IDs", func() {
	It("formats the UTC time plus 8 random hex digits", func() {
		id, err := backup.NewPointID(sampleTime.In(time.FixedZone("x", 3600)), bytes.NewReader([]byte{0xde, 0xad, 0xbe, 0xef}))
		Expect(err).NotTo(HaveOccurred())
		Expect(id).To(Equal("20261007T010000Z-deadbeef"))
		Expect(backup.ValidatePointID(id)).To(Succeed())
	})

	It("sorts by time", func() {
		a, _ := backup.NewPointID(sampleTime, bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff}))
		b, _ := backup.NewPointID(sampleTime.Add(time.Second), bytes.NewReader([]byte{0, 0, 0, 0}))
		Expect(a < b).To(BeTrue())
	})

	It("fails when randomness is unavailable", func() {
		_, err := backup.NewPointID(sampleTime, iotest.ErrReader(iotest.ErrTimeout))
		Expect(err).To(MatchError(iotest.ErrTimeout))
	})

	DescribeTable("rejects malformed IDs",
		func(id string) {
			Expect(backup.ValidatePointID(id)).To(MatchError(backup.ErrInvalidPointID))
		},
		Entry("empty", ""),
		Entry("traversal", "../20261007T010000Z-deadbeef"),
		Entry("uppercase suffix", "20261007T010000Z-DEADBEEF"),
		Entry("missing suffix", "20261007T010000Z"),
		Entry("slash", "20261007T010000Z-dead/eef"),
	)
})

var _ = Describe("Manifest encoding", func() {
	It("round-trips a valid manifest", func() {
		m, err := backup.Decode(bytes.NewReader(encode(sampleManifest())))
		Expect(err).NotTo(HaveOccurred())
		Expect(encode(m)).To(Equal(encode(sampleManifest())))
	})

	It("refuses to encode an invalid manifest", func() {
		m := sampleManifest()
		m.ID = "nope"
		Expect(backup.Encode(&bytes.Buffer{}, m)).To(MatchError(backup.ErrInvalidManifest))
	})

	DescribeTable("Decode rejects untrusted input with ErrInvalidManifest",
		func(mutate func(string) string) {
			_, err := backup.Decode(strings.NewReader(mutate(string(encode(sampleManifest())))))
			Expect(err).To(MatchError(backup.ErrInvalidManifest))
		},
		Entry("unknown field", func(s string) string { return strings.Replace(s, `"id"`, `"evil":1,"id"`, 1) }),
		Entry("trailing data", func(s string) string { return s + `{}` }),
		Entry("unsupported schema version", func(s string) string {
			return strings.Replace(s, `"schema_version": 1`, `"schema_version": 2`, 1)
		}),
		Entry("traversal object key", func(s string) string {
			return strings.Replace(s, `artifacts/a.json`, `../../etc/passwd`, 1)
		}),
		Entry("short sha256", func(s string) string { return strings.Replace(s, strings.Repeat("a", 64), "abc", 1) }),
		Entry("uppercase sha256", func(s string) string {
			return strings.Replace(s, strings.Repeat("a", 64), strings.Repeat("A", 64), 1)
		}),
		Entry("bad tenant ID", func(s string) string { return strings.Replace(s, `"acme-corp"`, `"Acme_Corp"`, 1) }),
		Entry("non-WAL-G backup name", func(s string) string {
			return strings.Replace(s, `base_000000010000000000000004`, `base_x; rm -rf /`, 1)
		}),
		Entry("unknown stream", func(s string) string { return strings.Replace(s, `"AUDIT_EVENTS"`, `"OTHER"`, 1) }),
		Entry("unexpected snapshot file", func(s string) string { return strings.Replace(s, `stream.arc.s2`, `../x`, 1) }),
		Entry("snapshot without data file", func(s string) string { return strings.Replace(s, `stream.arc.s2`, `backup.json`, 1) }),
		Entry("inconsistent stream state", func(s string) string {
			return strings.Replace(s, `"msgs": 5`, `"msgs": 9`, 1)
		}),
		Entry("negative bytes", func(s string) string { return strings.Replace(s, `"bytes": 1000`, `"bytes": -1`, 1) }),
		Entry("not JSON", func(string) string { return "not json" }),
		Entry("non-canonical key: dot segment", func(s string) string {
			return strings.Replace(s, "artifacts/a.json", "artifacts/./a.json", 1)
		}),
		Entry("non-canonical key: double slash", func(s string) string {
			return strings.Replace(s, "artifacts/a.json", "artifacts//a.json", 1)
		}),
		Entry("non-canonical key: leading dot-slash", func(s string) string {
			return strings.Replace(s, "artifacts/a.json", "./artifacts/a.json", 1)
		}),
		Entry("non-canonical key: trailing slash", func(s string) string {
			return strings.Replace(s, "artifacts/a.json", "artifacts/a.json/", 1)
		}),
	)

	It("rejects a manifest larger than the size cap", func() {
		DeferCleanup(backup.SetMaxManifestBytes(64))
		_, err := backup.Decode(strings.NewReader(`{"id":"` + strings.Repeat("x", 100) + `"}`))
		Expect(err).To(MatchError(backup.ErrInvalidManifest))
		Expect(err.Error()).To(ContainSubstring("exceeds 64 bytes"))
	})

	DescribeTable("Encode rejects non-finite throughput",
		func(throughput float64) {
			m := sampleManifest()
			m.Objects.ThroughputBytesPerSec = throughput
			Expect(backup.Encode(&bytes.Buffer{}, m)).To(MatchError(backup.ErrInvalidManifest))
		},
		Entry("positive infinity", math.Inf(1)),
		Entry("negative infinity", math.Inf(-1)),
		Entry("NaN", math.NaN()),
	)
})
