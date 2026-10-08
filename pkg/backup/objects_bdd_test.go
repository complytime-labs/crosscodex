package backup_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/backup"
	"github.com/complytime-labs/crosscodex/pkg/storage"
)

// errReadCloser fails every Read with readErr and every Close with
// closeErr, so specs can force both a spool failure and a close failure
// on the same handle.
type errReadCloser struct{ readErr, closeErr error }

func (e *errReadCloser) Read([]byte) (int, error) { return 0, e.readErr }
func (e *errReadCloser) Close() error             { return e.closeErr }

// getErrProvider wraps a storage.Provider and makes every Get return rc,
// so a spool-read failure and its Close failure can be injected without a
// dedicated fake Provider.
type getErrProvider struct {
	storage.Provider
	rc io.ReadCloser
}

func (p *getErrProvider) Get(context.Context, string) (io.ReadCloser, error) { return p.rc, nil }

// closeErrProvider wraps a storage.Provider and makes Close return closeErr
// regardless of the wrapped provider's own Close outcome.
type closeErrProvider struct {
	storage.Provider
	closeErr error
}

func (p *closeErrProvider) Close() error { return p.closeErr }

// objectStores is a local tenant object store root plus its opener.
type objectStores struct{ root string }

func newObjectStores() *objectStores { return &objectStores{root: GinkgoT().TempDir()} }

func (o *objectStores) open(tenantID string) (storage.Provider, error) {
	return storage.NewLocal(o.root, tenantID)
}

func (o *objectStores) put(tenantID, key, data string) {
	p, err := o.open(tenantID)
	Expect(err).NotTo(HaveOccurred())
	defer p.Close()
	Expect(p.Put(context.Background(), key, strings.NewReader(data))).To(Succeed())
}

// contents returns tenant -> key -> data for every object.
func (o *objectStores) contents(tenants ...string) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, t := range tenants {
		p, err := o.open(t)
		Expect(err).NotTo(HaveOccurred())
		objs, err := p.List(context.Background(), "")
		Expect(err).NotTo(HaveOccurred())
		out[t] = map[string]string{}
		for _, ob := range objs {
			rc, err := p.Get(context.Background(), ob.Key)
			Expect(err).NotTo(HaveOccurred())
			b, err := io.ReadAll(rc)
			Expect(err).NotTo(HaveOccurred())
			Expect(rc.Close()).To(Succeed())
			out[t][ob.Key] = string(b)
		}
		Expect(p.Close()).To(Succeed())
	}
	return out
}

func blobFiles(repoRoot string) []string {
	entries, err := os.ReadDir(filepath.Join(repoRoot, "backup", "objects", "blobs"))
	if os.IsNotExist(err) {
		return nil
	}
	Expect(err).NotTo(HaveOccurred())
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

var _ = Describe("Objects step", func() {
	ctx := context.Background()
	tenants := []string{"acme-corp", "globex"}

	var (
		src      *objectStores
		repo     *backup.Repository
		repoRoot string
	)

	BeforeEach(func() {
		src = newObjectStores()
		repo, repoRoot = newLocalRepo()
		src.put("acme-corp", "artifacts/a.json", "alpha")
		src.put("acme-corp", "artifacts/shared.json", "same")
		src.put("globex", "catalogs/c.json", "gamma")
		src.put("globex", "artifacts/shared.json", "same")
	})

	It("captures every tenant's objects and deduplicates identical content", func() {
		byTenant, total, err := backup.SnapshotObjects(ctx, repo, tenants, src.open, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(total).To(Equal(int64(len("alpha") + len("same") + len("gamma") + len("same"))))
		Expect(byTenant["acme-corp"]).To(HaveLen(2))
		Expect(byTenant["globex"]).To(HaveLen(2))
		Expect(byTenant["acme-corp"][1].SHA256).To(Equal(byTenant["globex"][0].SHA256), "same content, same blob")
		Expect(blobFiles(repoRoot)).To(HaveLen(3), "four objects, three distinct contents")
	})

	It("captures a tenant with no objects as an empty list", func() {
		byTenant, _, err := backup.SnapshotObjects(ctx, repo, []string{"initech"}, src.open, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(byTenant).To(HaveKeyWithValue("initech", BeEmpty()))
	})

	It("restores every object into empty stores byte for byte", func() {
		byTenant, _, err := backup.SnapshotObjects(ctx, repo, tenants, src.open, "")
		Expect(err).NotTo(HaveOccurred())
		want := src.contents(tenants...)

		dst := newObjectStores()
		Expect(backup.PreflightObjects(ctx, byTenant, dst.open)).To(Succeed())
		Expect(backup.RestoreObjects(ctx, repo, byTenant, dst.open, "")).To(Succeed())
		Expect(dst.contents(tenants...)).To(Equal(want))
	})

	It("refuses a non-empty tenant store, naming it and the fix, and writes nothing", func() {
		byTenant, _, err := backup.SnapshotObjects(ctx, repo, tenants, src.open, "")
		Expect(err).NotTo(HaveOccurred())
		dst := newObjectStores()
		dst.put("globex", "existing.json", "keep me")

		err = backup.PreflightObjects(ctx, byTenant, dst.open)
		Expect(err).To(MatchError(ContainSubstring(`tenant "globex"`)))
		Expect(err.Error()).To(ContainSubstring("existing.json"))
		Expect(err.Error()).To(ContainSubstring("empty"))
		Expect(dst.contents(tenants...)).To(Equal(map[string]map[string]string{
			"acme-corp": {}, "globex": {"existing.json": "keep me"},
		}))
	})

	It("verify reports a tampered blob and a missing blob", func() {
		byTenant, _, err := backup.SnapshotObjects(ctx, repo, tenants, src.open, "")
		Expect(err).NotTo(HaveOccurred())
		Expect(backup.VerifyObjects(ctx, repo, byTenant)).To(BeEmpty())

		names := blobFiles(repoRoot)
		Expect(os.WriteFile(filepath.Join(repoRoot, "backup", "objects", "blobs", names[0]), []byte("tampered"), 0o600)).To(Succeed())
		Expect(os.Remove(filepath.Join(repoRoot, "backup", "objects", "blobs", names[1]))).To(Succeed())

		issues, err := backup.VerifyObjects(ctx, repo, byTenant)
		Expect(err).NotTo(HaveOccurred())
		Expect(issues).To(ConsistOf(
			ContainSubstring("blob "+names[0]+" sha256 mismatch"),
			ContainSubstring("blob "+names[1]+" is missing"),
		))
	})

	It("restore refuses a blob whose content no longer matches, before writing it", func() {
		byTenant, _, err := backup.SnapshotObjects(ctx, repo, []string{"acme-corp"}, src.open, "")
		Expect(err).NotTo(HaveOccurred())
		sum := byTenant["acme-corp"][0].SHA256
		Expect(repo.PutBlob(ctx, sum, bytes.NewReader([]byte("evil")))).To(Succeed())

		dst := newObjectStores()
		err = backup.RestoreObjects(ctx, repo, byTenant, dst.open, "")
		Expect(err).To(MatchError(ContainSubstring("failed its checksum")))
		Expect(err.Error()).To(ContainSubstring("acme-corp"))
		Expect(err.Error()).To(ContainSubstring(byTenant["acme-corp"][0].Key))
		Expect(err.Error()).To(ContainSubstring("another point"))
		Expect(dst.contents("acme-corp")["acme-corp"]).NotTo(HaveKey(byTenant["acme-corp"][0].Key))
	})

	It("snapshot joins a spool-read failure with its close failure", func() {
		readErr := errors.New("spool read boom")
		closeErr := errors.New("source close boom")
		open := func(tenantID string) (storage.Provider, error) {
			base, err := src.open(tenantID)
			if err != nil {
				return nil, err
			}
			return &getErrProvider{Provider: base, rc: &errReadCloser{readErr: readErr, closeErr: closeErr}}, nil
		}
		_, _, err := backup.SnapshotObjects(ctx, repo, []string{"acme-corp"}, open, "")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(readErr.Error()))
		Expect(err.Error()).To(ContainSubstring(closeErr.Error()))
	})

	It("restore joins a spool-read failure with its close failure", func() {
		readErr := errors.New("spool read boom")
		closeErr := errors.New("blob close boom")
		repoStore, err := storage.NewLocal(GinkgoT().TempDir(), backup.Namespace)
		Expect(err).NotTo(HaveOccurred())
		badStore := &getErrProvider{Provider: repoStore, rc: &errReadCloser{readErr: readErr, closeErr: closeErr}}
		badRepo := backup.NewRepository(badStore)
		byTenant := map[string][]backup.ObjectEntry{
			"acme-corp": {{Key: "a.json", SHA256: strings.Repeat("a", 64), Size: 1}},
		}
		dst := newObjectStores()
		err = backup.RestoreObjects(ctx, badRepo, byTenant, dst.open, "")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(readErr.Error()))
		Expect(err.Error()).To(ContainSubstring(closeErr.Error()))
	})

	It("preflight reports a tenant's close failure separately from a list failure", func() {
		byTenant, _, err := backup.SnapshotObjects(ctx, repo, tenants, src.open, "")
		Expect(err).NotTo(HaveOccurred())

		closeErr := errors.New("close boom")
		open := func(tenantID string) (storage.Provider, error) {
			base, err := newObjectStores().open(tenantID)
			if err != nil {
				return nil, err
			}
			return &closeErrProvider{Provider: base, closeErr: closeErr}, nil
		}
		err = backup.PreflightObjects(ctx, byTenant, open)
		Expect(err).To(MatchError(closeErr))
		Expect(err.Error()).To(ContainSubstring("close object store"))
	})
})
