package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nats-io/jsm.go"
	"github.com/nats-io/jsm.go/api"
	"github.com/nats-io/nats.go"
)

// jsmStreamStore implements StreamStore with jsm.go, nats-io's reference
// snapshot/restore implementation (nats.go's jetstream package has no
// snapshot API). Snapshots keep message sequence numbers, which consumer
// replay would not; audit sequence numbers must survive a restore.
type jsmStreamStore struct {
	mgr *jsm.Manager
}

// NewJSMStreamStore wraps nc, a connection from natsbus.DialExternal.
func NewJSMStreamStore(nc *nats.Conn) (StreamStore, error) {
	mgr, err := jsm.New(nc)
	if err != nil {
		return nil, fmt.Errorf("create JetStream manager: %w", err)
	}
	return &jsmStreamStore{mgr: mgr}, nil
}

// Exists ignores ctx: jsm.go's lookup takes none (it uses the manager timeout).
func (s *jsmStreamStore) Exists(_ context.Context, stream string) (bool, error) {
	return s.mgr.IsKnownStream(stream)
}

// Snapshot asks the server to health-check the stream's messages first, so a
// corrupt stream fails the backup instead of producing a corrupt snapshot.
func (s *jsmStreamStore) Snapshot(ctx context.Context, stream, dir string) (StreamState, error) {
	st, err := s.mgr.LoadStream(stream)
	if err != nil {
		return StreamState{}, err
	}
	if _, err := st.SnapshotToDirectory(ctx, dir, jsm.SnapshotHealthCheck()); err != nil {
		return StreamState{}, err
	}
	return readSnapshotState(dir)
}

func (s *jsmStreamStore) Restore(ctx context.Context, stream, dir string) (StreamState, error) {
	if _, _, err := s.mgr.RestoreSnapshotFromDirectory(ctx, stream, dir); err != nil {
		return StreamState{}, err
	}
	st, err := s.mgr.LoadStream(stream)
	if err != nil {
		return StreamState{}, fmt.Errorf("load restored stream: %w", err)
	}
	state, err := st.State()
	if err != nil {
		return StreamState{}, fmt.Errorf("read restored stream state: %w", err)
	}
	return StreamState{FirstSeq: state.FirstSeq, LastSeq: state.LastSeq, Msgs: state.Msgs}, nil
}

// readSnapshotState reads the state jsm.go recorded in backup.json; it is
// the state the snapshot actually holds, unlike a later stream info call.
func readSnapshotState(dir string) (StreamState, error) {
	data, err := os.ReadFile(filepath.Join(dir, snapshotMetaFile))
	if err != nil {
		return StreamState{}, fmt.Errorf("read %s: %w", snapshotMetaFile, err)
	}
	var meta struct {
		State *api.StreamState `json:"state"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return StreamState{}, fmt.Errorf("decode %s: %w", snapshotMetaFile, err)
	}
	if meta.State == nil {
		return StreamState{}, errors.New(snapshotMetaFile + " has no state")
	}
	return StreamState{FirstSeq: meta.State.FirstSeq, LastSeq: meta.State.LastSeq, Msgs: meta.State.Msgs}, nil
}
