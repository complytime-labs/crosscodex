package backup_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/backup"
)

// fakeStreamStore keeps streams in memory. A snapshot writes backup.json
// (holding the state) and stream.arc.s2 (holding data); Restore reads them back.
type fakeStreamStore struct {
	streams  map[string]fakeStream
	restored []string
}

type fakeStream struct {
	state backup.StreamState
	data  string
}

func newFakeStreamStore() *fakeStreamStore {
	return &fakeStreamStore{streams: map[string]fakeStream{}}
}

func (f *fakeStreamStore) Exists(_ context.Context, stream string) (bool, error) {
	_, ok := f.streams[stream]
	return ok, nil
}

func (f *fakeStreamStore) Snapshot(_ context.Context, stream, dir string) (backup.StreamState, error) {
	s, ok := f.streams[stream]
	if !ok {
		return backup.StreamState{}, errors.New("no such stream")
	}
	meta, err := json.Marshal(s.state)
	if err != nil {
		return backup.StreamState{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "backup.json"), meta, 0o600); err != nil {
		return backup.StreamState{}, err
	}
	return s.state, os.WriteFile(filepath.Join(dir, "stream.arc.s2"), []byte(s.data), 0o600)
}

func (f *fakeStreamStore) Restore(_ context.Context, stream, dir string) (backup.StreamState, error) {
	meta, err := os.ReadFile(filepath.Join(dir, "backup.json"))
	if err != nil {
		return backup.StreamState{}, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "stream.arc.s2"))
	if err != nil {
		return backup.StreamState{}, err
	}
	var st backup.StreamState
	if err := json.Unmarshal(meta, &st); err != nil {
		return backup.StreamState{}, err
	}
	f.streams[stream] = fakeStream{state: st, data: string(data)}
	f.restored = append(f.restored, stream)
	return st, nil
}

var _ = Describe("JetStream step", func() {
	ctx := context.Background()
	const pointID = "20261007T010000Z-0a1b2c3d"
	names := []string{"AUDIT_LLM", "AUDIT_DECISIONS", "AUDIT_EVENTS"}

	var (
		src      *fakeStreamStore
		repo     *backup.Repository
		repoRoot string
	)

	BeforeEach(func() {
		src = newFakeStreamStore()
		for i, n := range names {
			src.streams[n] = fakeStream{state: backup.StreamState{FirstSeq: 1, LastSeq: uint64(i + 3), Msgs: uint64(i + 3)}, data: "data-" + n}
		}
		repo, repoRoot = newLocalRepo()
	})

	It("snapshots every audit stream with checksummed files", func() {
		caps, total, err := backup.SnapshotStreams(ctx, repo, pointID, src, names, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(caps).To(HaveLen(3))
		Expect(total).To(BeNumerically(">", 0))
		Expect(caps[2].Stream).To(Equal("AUDIT_EVENTS"))
		Expect(caps[2].State).To(Equal(backup.StreamState{FirstSeq: 1, LastSeq: 5, Msgs: 5}))
		Expect(caps[2].Files).To(HaveLen(2))
		Expect(filepath.Join(repoRoot, "backup", "nats", pointID, "AUDIT_EVENTS", "stream.arc.s2")).To(BeAnExistingFile())
	})

	It("fails with the fix when an audit stream does not exist", func() {
		delete(src.streams, "AUDIT_LLM")
		_, _, err := backup.SnapshotStreams(ctx, repo, pointID, src, names, "")
		Expect(err).To(MatchError(ContainSubstring(`"AUDIT_LLM" does not exist`)))
		Expect(err.Error()).To(ContainSubstring("Start crosscodexd once"))
	})

	It("restores into an empty JetStream and checks the restored state", func() {
		caps, _, err := backup.SnapshotStreams(ctx, repo, pointID, src, names, "")
		Expect(err).NotTo(HaveOccurred())
		dst := newFakeStreamStore()
		Expect(backup.PreflightStreams(ctx, caps, dst)).To(Succeed())
		Expect(backup.RestoreStreams(ctx, repo, pointID, caps, dst, "")).To(Succeed())
		Expect(dst.streams).To(Equal(src.streams))
	})

	It("refuses an existing stream, naming it and the fix, without restoring anything", func() {
		caps, _, err := backup.SnapshotStreams(ctx, repo, pointID, src, names, "")
		Expect(err).NotTo(HaveOccurred())
		dst := newFakeStreamStore()
		dst.streams["AUDIT_DECISIONS"] = fakeStream{data: "live"}
		err = backup.PreflightStreams(ctx, caps, dst)
		Expect(err).To(MatchError(ContainSubstring(`stream "AUDIT_DECISIONS"`)))
		Expect(err.Error()).To(ContainSubstring("nats stream rm AUDIT_DECISIONS"))
		Expect(dst.restored).To(BeEmpty())
		Expect(dst.streams["AUDIT_DECISIONS"].data).To(Equal("live"))
	})

	It("rejects a restore whose resulting state differs from the manifest", func() {
		caps, _, err := backup.SnapshotStreams(ctx, repo, pointID, src, names[:1], "")
		Expect(err).NotTo(HaveOccurred())
		caps[0].State.Msgs++ // manifest claims one more message than the snapshot holds
		err = backup.RestoreStreams(ctx, repo, pointID, caps, newFakeStreamStore(), "")
		Expect(err).To(MatchError(ContainSubstring("but does not match the manifest")))
		Expect(err.Error()).To(ContainSubstring("nats stream rm AUDIT_LLM"))
	})

	It("verify reports tampered and missing snapshot files", func() {
		caps, _, err := backup.SnapshotStreams(ctx, repo, pointID, src, names, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(backup.VerifyStreams(ctx, repo, pointID, caps)).To(BeEmpty())

		dir := filepath.Join(repoRoot, "backup", "nats", pointID)
		Expect(os.WriteFile(filepath.Join(dir, "AUDIT_LLM", "stream.arc.s2"), []byte("x"), 0o600)).To(Succeed())
		Expect(os.Remove(filepath.Join(dir, "AUDIT_EVENTS", "backup.json"))).To(Succeed())
		issues, err := backup.VerifyStreams(ctx, repo, pointID, caps)
		Expect(err).NotTo(HaveOccurred())
		Expect(issues).To(ConsistOf(
			ContainSubstring(`snapshot file stream.arc.s2 of stream "AUDIT_LLM" sha256 mismatch`),
			ContainSubstring(`snapshot file backup.json of stream "AUDIT_EVENTS" is missing`),
		))
	})
})
