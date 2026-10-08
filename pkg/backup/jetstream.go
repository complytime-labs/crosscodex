package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/complytime-labs/crosscodex/pkg/storage"
)

// StreamStore is the JetStream surface backups need. NewJSMStreamStore
// implements it with jsm.go.
type StreamStore interface {
	// Exists reports whether stream exists on the server.
	Exists(ctx context.Context, stream string) (bool, error)
	// Snapshot writes a snapshot of stream into the empty directory dir and
	// returns the state the snapshot holds.
	Snapshot(ctx context.Context, stream, dir string) (StreamState, error)
	// Restore recreates stream from the snapshot files in dir and returns the
	// stream's state afterwards.
	Restore(ctx context.Context, stream, dir string) (StreamState, error)
}

// snapshotStreams snapshots each named stream and uploads its files.
func snapshotStreams(ctx context.Context, repo *Repository, pointID string, store StreamStore, names []string, tmpDir string) ([]StreamCapture, int64, error) {
	caps := make([]StreamCapture, 0, len(names))
	var total int64
	for _, name := range names {
		c, err := snapshotStream(ctx, repo, pointID, store, name, tmpDir)
		if err != nil {
			return nil, 0, err
		}
		for _, f := range c.Files {
			total += f.Size
		}
		caps = append(caps, c)
	}
	return caps, total, nil
}

func snapshotStream(ctx context.Context, repo *Repository, pointID string, store StreamStore, name, tmpDir string) (c StreamCapture, err error) {
	exists, err := store.Exists(ctx, name)
	if err != nil {
		return StreamCapture{}, fmt.Errorf("look up stream %q: %w", name, err)
	}
	if !exists {
		return StreamCapture{}, fmt.Errorf("audit stream %q does not exist on the NATS server. Start crosscodexd once against this server so it creates the audit streams, then retry", name)
	}
	dir, err := os.MkdirTemp(tmpDir, "crosscodex-snapshot-*")
	if err != nil {
		return StreamCapture{}, fmt.Errorf("create snapshot directory: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	state, err := store.Snapshot(ctx, name, dir)
	if err != nil {
		return StreamCapture{}, fmt.Errorf("snapshot stream %q: %w", name, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return StreamCapture{}, fmt.Errorf("read snapshot of stream %q: %w", name, err)
	}
	c = StreamCapture{Stream: name, State: state}
	for _, de := range entries {
		if !de.Type().IsRegular() {
			return StreamCapture{}, fmt.Errorf("snapshot of stream %q holds unexpected entry %q", name, de.Name())
		}
		f, err := uploadStreamFile(ctx, repo, pointID, name, filepath.Join(dir, de.Name()))
		if err != nil {
			return StreamCapture{}, err
		}
		c.Files = append(c.Files, f)
	}
	return c, nil
}

func uploadStreamFile(ctx context.Context, repo *Repository, pointID, stream, path string) (sf StreamFile, err error) {
	name := filepath.Base(path)
	f, err := os.Open(path)
	if err != nil {
		return StreamFile{}, fmt.Errorf("open snapshot file %s of stream %q: %w", name, stream, err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	sum, size, err := hashReader(f)
	if err != nil {
		return StreamFile{}, fmt.Errorf("hash snapshot file %s of stream %q: %w", name, stream, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return StreamFile{}, fmt.Errorf("rewind snapshot file %s of stream %q: %w", name, stream, err)
	}
	if err := repo.PutStreamFile(ctx, pointID, stream, name, f); err != nil {
		return StreamFile{}, fmt.Errorf("upload snapshot file %s of stream %q: %w", name, stream, err)
	}
	return StreamFile{Name: name, SHA256: sum, Size: size}, nil
}

// preflightStreams refuses if any target stream exists. Restore runs it
// before writing anything, so a refusal changes nothing.
func preflightStreams(ctx context.Context, caps []StreamCapture, store StreamStore) error {
	for _, c := range caps {
		exists, err := store.Exists(ctx, c.Stream)
		if err != nil {
			return fmt.Errorf("look up stream %q: %w", c.Stream, err)
		}
		if exists {
			return fmt.Errorf("cannot restore stream %q: it already exists on the NATS server. Restore never overwrites a stream; delete it (nats stream rm %s) or restore into an empty JetStream, then retry", c.Stream, c.Stream)
		}
	}
	return nil
}

// restoreStreams downloads and checks each snapshot, restores it, and
// checks the restored state against the manifest.
func restoreStreams(ctx context.Context, repo *Repository, pointID string, caps []StreamCapture, store StreamStore, tmpDir string) error {
	for _, c := range caps {
		if err := restoreStream(ctx, repo, pointID, c, store, tmpDir); err != nil {
			return err
		}
	}
	return nil
}

// restoreStream restores one snapshot and checks the restored state against
// the manifest. This check runs after store.Restore has already created the
// stream, so it can only detect a mismatch, not prevent one. That is safe
// because restoreStreams only runs after preflightStreams has confirmed
// every target stream is absent: on a mismatch, the stream this call just
// created is the only change restore made, so naming it is enough to undo.
func restoreStream(ctx context.Context, repo *Repository, pointID string, c StreamCapture, store StreamStore, tmpDir string) (err error) {
	dir, err := os.MkdirTemp(tmpDir, "crosscodex-restore-*")
	if err != nil {
		return fmt.Errorf("create restore directory: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	for _, f := range c.Files {
		if err := downloadStreamFile(ctx, repo, pointID, c.Stream, f, dir); err != nil {
			return err
		}
	}
	got, err := store.Restore(ctx, c.Stream, dir)
	if err != nil {
		return fmt.Errorf("restore stream %q: %w", c.Stream, err)
	}
	if got != c.State {
		return fmt.Errorf("stream %q was restored but does not match the manifest: got first_seq=%d last_seq=%d msgs=%d, want first_seq=%d last_seq=%d msgs=%d. Delete it (nats stream rm %s) and restore from another point",
			c.Stream, got.FirstSeq, got.LastSeq, got.Msgs, c.State.FirstSeq, c.State.LastSeq, c.State.Msgs, c.Stream)
	}
	return nil
}

// downloadStreamFile writes one snapshot file into dir and checks its hash.
// sf.Name comes from a validated manifest (a fixed allowlist), so joining it
// to dir cannot escape dir.
func downloadStreamFile(ctx context.Context, repo *Repository, pointID, stream string, sf StreamFile, dir string) (err error) {
	rc, err := repo.OpenStreamFile(ctx, pointID, stream, sf.Name)
	if err != nil {
		return fmt.Errorf("read snapshot file %s of stream %q: %w", sf.Name, stream, err)
	}
	defer func() { err = errors.Join(err, rc.Close()) }()
	out, err := os.OpenFile(filepath.Join(dir, sf.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create snapshot file %s of stream %q: %w", sf.Name, stream, err)
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), rc)
	if err != nil {
		return fmt.Errorf("download snapshot file %s of stream %q: %w", sf.Name, stream, err)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != sf.SHA256 || n != sf.Size {
		return fmt.Errorf("snapshot file %s of stream %q failed its checksum (content is %d bytes with sha256 %s); the repository is damaged. Run `crosscodexd admin backup verify` and restore from another point",
			sf.Name, stream, n, sum)
	}
	return nil
}

// verifyStreams re-hashes every snapshot file of a point.
func verifyStreams(ctx context.Context, repo *Repository, pointID string, caps []StreamCapture) ([]string, error) {
	var issues []string
	for _, c := range caps {
		for _, f := range c.Files {
			rc, err := repo.OpenStreamFile(ctx, pointID, c.Stream, f.Name)
			if errors.Is(err, storage.ErrNotFound) {
				issues = append(issues, fmt.Sprintf("snapshot file %s of stream %q is missing", f.Name, c.Stream))
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("read snapshot file %s of stream %q: %w", f.Name, c.Stream, err)
			}
			sum, size, err := hashReader(rc)
			err = errors.Join(err, rc.Close())
			if err != nil {
				return nil, fmt.Errorf("read snapshot file %s of stream %q: %w", f.Name, c.Stream, err)
			}
			if sum != f.SHA256 || size != f.Size {
				issues = append(issues, fmt.Sprintf("snapshot file %s of stream %q sha256 mismatch: content is %d bytes hashing to %s", f.Name, c.Stream, size, sum))
			}
		}
	}
	return issues, nil
}
