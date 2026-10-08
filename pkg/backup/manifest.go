package backup

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"regexp"
	"slices"
	"time"

	"github.com/complytime-labs/crosscodex/pkg/natsbus"
	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// SchemaVersion is the manifest format this package writes and accepts.
const SchemaVersion = 1

// MaxManifestBytes bounds how much Decode reads. A manifest is untrusted
// input; the bound keeps a corrupt or hostile file from exhausting memory.
// About 150 bytes per object entry puts the cap near 1.7 million objects.
const MaxManifestBytes = 256 << 20

// maxManifestBytes is the cap Decode applies; specs lower it via export_test.go.
var maxManifestBytes int64 = MaxManifestBytes

var (
	// ErrInvalidManifest wraps every manifest decoding or validation failure.
	ErrInvalidManifest = errors.New("invalid backup manifest")
	// ErrInvalidPointID wraps a malformed backup point ID.
	ErrInvalidPointID = errors.New("invalid backup point ID")
)

var (
	pointIDPattern    = regexp.MustCompile(`^\d{8}T\d{6}Z-[0-9a-f]{8}$`)
	sha256Pattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	walgBackupPattern = regexp.MustCompile(`^base_[0-9A-F]{24}$`)
)

// Files jsm.go's SnapshotToDirectory writes: metadata plus one data
// archive, named stream.arc.s2 on nats-server >= 2.15 and stream.tar.s2 before.
const (
	snapshotMetaFile       = "backup.json"
	snapshotDataFile       = "stream.arc.s2"
	snapshotLegacyDataFile = "stream.tar.s2"
)

// Manifest describes one backup point.
type Manifest struct {
	SchemaVersion int             `json:"schema_version"`
	ID            string          `json:"id"`
	StartedAt     time.Time       `json:"started_at"`
	FinishedAt    time.Time       `json:"finished_at"`
	Postgres      PostgresCapture `json:"postgres"`
	Objects       ObjectsCapture  `json:"objects"`
	NATS          NATSCapture     `json:"nats"`
}

// Window is the wall-clock interval a store was captured in.
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// CaptureStats is common to every store's capture. Throughput is the
// capture rate and estimates the restore rate.
type CaptureStats struct {
	Window                Window  `json:"window"`
	Bytes                 int64   `json:"bytes"`
	ThroughputBytesPerSec float64 `json:"throughput_bytes_per_sec"`
}

// PostgresCapture records the WAL-G base backup taken for this point.
type PostgresCapture struct {
	BackupName string `json:"backup_name"`
	StartLSN   uint64 `json:"start_lsn"`
	FinishLSN  uint64 `json:"finish_lsn"`
	Timeline   uint32 `json:"timeline"`
	CaptureStats
}

// ObjectEntry is one tenant object; its content is blob SHA256.
type ObjectEntry struct {
	Key    string `json:"key"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// ObjectsCapture lists every tenant's objects.
type ObjectsCapture struct {
	Tenants map[string][]ObjectEntry `json:"tenants"`
	CaptureStats
}

// StreamFile is one file of a stream snapshot.
type StreamFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// StreamState is a stream's sequence state at snapshot time.
type StreamState struct {
	FirstSeq uint64 `json:"first_seq"`
	LastSeq  uint64 `json:"last_seq"`
	Msgs     uint64 `json:"msgs"`
}

// StreamCapture is one audit stream's snapshot.
type StreamCapture struct {
	Stream string       `json:"stream"`
	Files  []StreamFile `json:"files"`
	State  StreamState  `json:"state"`
}

// NATSCapture lists the audit stream snapshots.
type NATSCapture struct {
	Streams []StreamCapture `json:"streams"`
	CaptureStats
}

// NewPointID returns now as YYYYMMDDTHHMMSSZ (UTC) plus 8 random hex digits
// read from r, so IDs sort by time and concurrent runs don't collide.
func NewPointID(now time.Time, r io.Reader) (string, error) {
	b := make([]byte, 4)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("generate backup point ID: %w", err)
	}
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b), nil
}

// ValidatePointID checks the point ID format. IDs become repository keys,
// so this is also the path-safety check.
func ValidatePointID(id string) error {
	if !pointIDPattern.MatchString(id) {
		return fmt.Errorf("%w: %q does not match YYYYMMDDTHHMMSSZ-xxxxxxxx; copy an ID from `crosscodexd admin backup list`", ErrInvalidPointID, id)
	}
	return nil
}

// Encode validates m and writes it as indented JSON.
func Encode(w io.Writer, m *Manifest) error {
	if err := m.Validate(); err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return fmt.Errorf("encode backup manifest: %w", err)
	}
	return nil
}

// Decode reads one manifest from untrusted input: at most MaxManifestBytes,
// no unknown fields, no trailing data, then full validation.
func Decode(r io.Reader) (*Manifest, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read backup manifest: %w", err)
	}
	if int64(len(data)) > maxManifestBytes {
		return nil, invalidf("manifest exceeds %d bytes", maxManifestBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, invalidf("trailing data after the manifest object")
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks every field Restore and Verify rely on. Every failure
// wraps ErrInvalidManifest.
func (m *Manifest) Validate() error {
	if m.SchemaVersion != SchemaVersion {
		return invalidf("schema_version %d is not supported (this build reads %d)", m.SchemaVersion, SchemaVersion)
	}
	if err := ValidatePointID(m.ID); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidManifest, err)
	}
	if m.StartedAt.IsZero() || m.FinishedAt.Before(m.StartedAt) {
		return invalidf("started_at must be set and not after finished_at")
	}
	if err := m.Postgres.validate(); err != nil {
		return err
	}
	if err := m.Objects.validate(); err != nil {
		return err
	}
	return m.NATS.validate()
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidManifest, fmt.Sprintf(format, args...))
}

func (s CaptureStats) validate(store string) error {
	if s.Window.Start.IsZero() || s.Window.End.Before(s.Window.Start) {
		return invalidf("%s.window must be set and start no later than it ends", store)
	}
	if s.Bytes < 0 || s.ThroughputBytesPerSec < 0 {
		return invalidf("%s bytes and throughput must not be negative", store)
	}
	if math.IsNaN(s.ThroughputBytesPerSec) || math.IsInf(s.ThroughputBytesPerSec, 0) {
		return invalidf("%s throughput must be finite", store)
	}
	return nil
}

func (p PostgresCapture) validate() error {
	if !walgBackupPattern.MatchString(p.BackupName) {
		return invalidf("postgres.backup_name %q is not a WAL-G base backup name (base_ plus 24 hex digits)", p.BackupName)
	}
	if p.Timeline == 0 {
		return invalidf("postgres.timeline must be at least 1")
	}
	if p.FinishLSN < p.StartLSN {
		return invalidf("postgres.finish_lsn precedes start_lsn")
	}
	return p.CaptureStats.validate("postgres")
}

func (o ObjectsCapture) validate() error {
	for tenantID, entries := range o.Tenants {
		if err := tenant.ValidateTenantID(tenantID); err != nil {
			return invalidf("objects.tenants: %v", err)
		}
		seen := make(map[string]bool, len(entries))
		for _, e := range entries {
			if err := storage.ValidateKey(e.Key); err != nil {
				return invalidf("objects.tenants[%q]: %v", tenantID, err)
			}
			if path.Clean(e.Key) != e.Key {
				return invalidf("objects.tenants[%q]: key %q is not in canonical form", tenantID, e.Key)
			}
			if seen[e.Key] {
				return invalidf("objects.tenants[%q]: duplicate key %q", tenantID, e.Key)
			}
			seen[e.Key] = true
			if !sha256Pattern.MatchString(e.SHA256) {
				return invalidf("objects.tenants[%q] key %q: sha256 must be 64 lowercase hex digits", tenantID, e.Key)
			}
			if e.Size < 0 {
				return invalidf("objects.tenants[%q] key %q: negative size", tenantID, e.Key)
			}
		}
	}
	return o.CaptureStats.validate("objects")
}

func (n NATSCapture) validate() error {
	known := natsbus.AuditStreamNames()
	seen := make(map[string]bool, len(n.Streams))
	for _, s := range n.Streams {
		if !slices.Contains(known, s.Stream) {
			return invalidf("nats.streams: %q is not an audit stream (want one of %v)", s.Stream, known)
		}
		if seen[s.Stream] {
			return invalidf("nats.streams: duplicate stream %q", s.Stream)
		}
		seen[s.Stream] = true
		if err := s.validate(); err != nil {
			return err
		}
	}
	return n.CaptureStats.validate("nats")
}

func (s StreamCapture) validate() error {
	names := make(map[string]bool, len(s.Files))
	for _, f := range s.Files {
		if f.Name != snapshotMetaFile && f.Name != snapshotDataFile && f.Name != snapshotLegacyDataFile {
			return invalidf("nats stream %q: unexpected snapshot file %q", s.Stream, f.Name)
		}
		if names[f.Name] {
			return invalidf("nats stream %q: duplicate snapshot file %q", s.Stream, f.Name)
		}
		names[f.Name] = true
		if !sha256Pattern.MatchString(f.SHA256) || f.Size < 0 {
			return invalidf("nats stream %q file %q: sha256 must be 64 lowercase hex digits and size non-negative", s.Stream, f.Name)
		}
	}
	if !names[snapshotMetaFile] || names[snapshotDataFile] == names[snapshotLegacyDataFile] {
		return invalidf("nats stream %q: snapshot must hold %s and exactly one of %s or %s",
			s.Stream, snapshotMetaFile, snapshotDataFile, snapshotLegacyDataFile)
	}
	st := s.State
	if st.Msgs > 0 && (st.FirstSeq == 0 || st.LastSeq < st.FirstSeq || st.Msgs > st.LastSeq-st.FirstSeq+1) {
		return invalidf("nats stream %q: state first_seq=%d last_seq=%d msgs=%d is inconsistent",
			s.Stream, st.FirstSeq, st.LastSeq, st.Msgs)
	}
	return nil
}
