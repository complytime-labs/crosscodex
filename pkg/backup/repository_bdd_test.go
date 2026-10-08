package backup_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/backup"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/storage"
)

// newLocalRepo returns a repository on a temp dir and the dir.
func newLocalRepo() (*backup.Repository, string) {
	root := GinkgoT().TempDir()
	store, err := storage.NewLocal(root, backup.Namespace)
	Expect(err).NotTo(HaveOccurred())
	repo := backup.NewRepository(store)
	DeferCleanup(repo.Close)
	return repo, root
}

var _ = Describe("Repository", func() {
	ctx := context.Background()

	It("opens a local destination under <path>/backup", func() {
		root := GinkgoT().TempDir()
		repo, err := backup.OpenRepository(config.BackupDestinationConfig{Backend: "local", Local: config.BackupLocalConfig{Path: root}})
		Expect(err).NotTo(HaveOccurred())
		defer repo.Close()
		Expect(repo.Begin(ctx, "20261007T010000Z-0a1b2c3d", sampleTime)).To(Succeed())
		Expect(filepath.Join(root, "backup", "points", "20261007T010000Z-0a1b2c3d", "IN_PROGRESS")).To(BeAnExistingFile())
	})

	It("fails closed when backup is disabled", func() {
		_, err := backup.OpenRepository(config.BackupDestinationConfig{})
		Expect(err).To(MatchError(ContainSubstring("backup is disabled")))
	})

	It("treats a point as complete only once its manifest exists", func() {
		repo, _ := newLocalRepo()
		m := sampleManifest()
		Expect(repo.Begin(ctx, m.ID, m.StartedAt)).To(Succeed())

		pts, err := repo.Points(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(pts.Complete).To(BeEmpty())
		Expect(pts.Incomplete).To(Equal([]string{m.ID}))

		Expect(repo.WriteManifest(ctx, m)).To(Succeed())
		pts, err = repo.Points(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(pts.Complete).To(Equal([]string{m.ID}), "manifest wins over a stale IN_PROGRESS")
		Expect(pts.Incomplete).To(BeEmpty())

		Expect(repo.ClearInProgress(ctx, m.ID)).To(Succeed())
		got, err := repo.LoadManifest(ctx, m.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.ID).To(Equal(m.ID))
	})

	It("lists points in time order", func() {
		repo, _ := newLocalRepo()
		for _, id := range []string{"20261008T000000Z-00000000", "20261007T000000Z-ffffffff"} {
			Expect(repo.Begin(ctx, id, sampleTime)).To(Succeed())
		}
		pts, err := repo.Points(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(pts.Incomplete).To(Equal([]string{"20261007T000000Z-ffffffff", "20261008T000000Z-00000000"}))
	})

	It("rejects an unexpected directory under points/", func() {
		repo, root := newLocalRepo()
		Expect(os.MkdirAll(filepath.Join(root, "backup", "points", "evil"), 0o700)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(root, "backup", "points", "evil", "manifest.json"), []byte("{}"), 0o600)).To(Succeed())
		_, err := repo.Points(ctx)
		Expect(err).To(MatchError(backup.ErrInvalidPointID))
		Expect(err.Error()).To(ContainSubstring("move it out"))
	})

	It("reports a missing manifest with the fix", func() {
		repo, _ := newLocalRepo()
		_, err := repo.LoadManifest(ctx, "20261007T010000Z-0a1b2c3d")
		Expect(err).To(MatchError(storage.ErrNotFound))
		Expect(err.Error()).To(ContainSubstring("admin backup list"))
	})

	It("rejects a manifest whose ID differs from its directory", func() {
		repo, root := newLocalRepo()
		other := "20261007T020000Z-0a1b2c3d"
		dir := filepath.Join(root, "backup", "points", other)
		Expect(os.MkdirAll(dir, 0o700)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "manifest.json"), encode(sampleManifest()), 0o600)).To(Succeed())
		_, err := repo.LoadManifest(ctx, other)
		Expect(err).To(MatchError(backup.ErrInvalidManifest))
		Expect(err.Error()).To(ContainSubstring("modified outside crosscodexd"))
	})

	DescribeTable("rejects a point whose manifest fails to decode",
		func(content string) {
			repo, root := newLocalRepo()
			id := "20261007T010000Z-0a1b2c3d"
			dir := filepath.Join(root, "backup", "points", id)
			Expect(os.MkdirAll(dir, 0o700)).To(Succeed())
			Expect(os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(content), 0o600)).To(Succeed())
			_, err := repo.LoadManifest(ctx, id)
			Expect(err).To(MatchError(backup.ErrInvalidManifest))
			Expect(err.Error()).To(ContainSubstring(id))
		},
		Entry("empty object", `{}`),
		Entry("truncated JSON", `{"schema_version":1,"id":"20261007T010000Z-0a1b2c3d",`),
	)

	It("stores blobs by sha256 and stream files by point", func() {
		repo, root := newLocalRepo()
		sum := strings.Repeat("d", 64)
		ok, err := repo.HasBlob(ctx, sum)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
		Expect(repo.PutBlob(ctx, sum, bytes.NewReader([]byte("x")))).To(Succeed())
		Expect(filepath.Join(root, "backup", "objects", "blobs", sum)).To(BeAnExistingFile())
		rc, err := repo.OpenBlob(ctx, sum)
		Expect(err).NotTo(HaveOccurred())
		Expect(io.ReadAll(rc)).To(Equal([]byte("x")))
		Expect(rc.Close()).To(Succeed())

		id := "20261007T010000Z-0a1b2c3d"
		Expect(repo.PutStreamFile(ctx, id, "AUDIT_EVENTS", "backup.json", strings.NewReader("{}"))).To(Succeed())
		Expect(filepath.Join(root, "backup", "nats", id, "AUDIT_EVENTS", "backup.json")).To(BeAnExistingFile())
	})

	It("rejects ClearInProgress on a malformed point ID, touching nothing", func() {
		repo, root := newLocalRepo()
		err := repo.ClearInProgress(ctx, "../evil")
		Expect(err).To(MatchError(backup.ErrInvalidPointID))
		Expect(filepath.Join(root, "backup", "points")).NotTo(BeAnExistingFile())
	})

	validID := "20261007T010000Z-0a1b2c3d"
	validSum := strings.Repeat("d", 64)

	DescribeTable("rejects a blob name that is not a sha256 hex digest",
		func(sum string) {
			repo, root := newLocalRepo()

			_, err := repo.HasBlob(ctx, sum)
			Expect(err).To(MatchError(backup.ErrInvalidManifest))
			Expect(err.Error()).To(ContainSubstring(sum))

			err = repo.PutBlob(ctx, sum, strings.NewReader("x"))
			Expect(err).To(MatchError(backup.ErrInvalidManifest))

			_, err = repo.OpenBlob(ctx, sum)
			Expect(err).To(MatchError(backup.ErrInvalidManifest))

			Expect(filepath.Join(root, "backup", "objects", "blobs")).NotTo(BeAnExistingFile())
		},
		Entry("empty", ""),
		Entry("too short", "abc"),
		Entry("uppercase", strings.Repeat("D", 64)),
		Entry("non-hex", strings.Repeat("g", 64)),
		Entry("traversal", "../../etc/passwd"),
	)

	DescribeTable("rejects a stream file reference outside the audit contract",
		func(id, stream, name string) {
			repo, root := newLocalRepo()

			err := repo.PutStreamFile(ctx, id, stream, name, strings.NewReader("x"))
			Expect(err).To(HaveOccurred())

			_, err = repo.OpenStreamFile(ctx, id, stream, name)
			Expect(err).To(HaveOccurred())

			Expect(filepath.Join(root, "backup", "nats")).NotTo(BeAnExistingFile())
		},
		Entry("malformed point ID", "../evil", "AUDIT_EVENTS", "backup.json"),
		Entry("unknown stream", validID, "NOT_A_STREAM", "backup.json"),
		Entry("unexpected file name", validID, "AUDIT_EVENTS", "../x"),
		Entry("unexpected file name 2", validID, "AUDIT_EVENTS", "evil.txt"),
	)

	It("accepts a valid blob name and stream file reference", func() {
		repo, _ := newLocalRepo()
		_, err := repo.HasBlob(ctx, validSum)
		Expect(err).NotTo(HaveOccurred())
		Expect(repo.PutStreamFile(ctx, validID, "AUDIT_EVENTS", "backup.json", strings.NewReader("{}"))).To(Succeed())
	})
})
