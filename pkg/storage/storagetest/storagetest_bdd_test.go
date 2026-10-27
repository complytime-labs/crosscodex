package storagetest_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
)

func TestStorageTest(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "StorageTest Suite")
}

var errScripted = errors.New("scripted")

// invalidKeys are keys storage.ValidateKey rejects.
var invalidKeys = []string{"", "/abs", "../up", "a/../b", "a\\b", "a\x00b", "."}

func readAll(rc io.ReadCloser) []byte {
	GinkgoHelper()
	defer func() { Expect(rc.Close()).To(Succeed()) }()
	b, err := io.ReadAll(rc)
	Expect(err).NotTo(HaveOccurred())
	return b
}

func put(ctx context.Context, p storage.Provider, key, data string) {
	GinkgoHelper()
	Expect(p.Put(ctx, key, strings.NewReader(data))).To(Succeed())
}

func listKeys(ctx context.Context, p storage.Provider, prefix string) []string {
	GinkgoHelper()
	metas, err := p.List(ctx, prefix)
	Expect(err).NotTo(HaveOccurred())
	keys := make([]string, 0, len(metas))
	for _, m := range metas {
		keys = append(keys, m.Key)
	}
	return keys
}

// Every spec in this table runs against both storage.NewLocal and Fake: the
// table is the proof that Fake behaves as the real provider does.
var _ = DescribeTable("Provider parity with storage.NewLocal",
	func(newProvider func() storage.Provider) {
		ctx := context.Background()
		p := newProvider()

		By("reporting missing keys as production does")
		_, err := p.Get(ctx, "missing")
		Expect(err).To(MatchError(storage.ErrNotFound))
		_, err = p.Stat(ctx, "missing")
		Expect(err).To(MatchError(storage.ErrNotFound))
		Expect(p.Delete(ctx, "missing")).To(Succeed())
		ok, err := p.Exists(ctx, "missing")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
		Expect(listKeys(ctx, p, "")).To(BeEmpty())
		Expect(listKeys(ctx, p, "nothing/here")).To(BeEmpty())

		By("round-tripping and overwriting")
		put(ctx, p, "docs/a.txt", "one")
		rc, err := p.Get(ctx, "docs/a.txt")
		Expect(err).NotTo(HaveOccurred())
		Expect(readAll(rc)).To(Equal([]byte("one")))
		put(ctx, p, "docs/a.txt", "three")
		rc, err = p.Get(ctx, "docs/a.txt")
		Expect(err).NotTo(HaveOccurred())
		Expect(readAll(rc)).To(Equal([]byte("three")))
		ok, err = p.Exists(ctx, "docs/a.txt")
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())

		By("populating Stat the way production does")
		meta, err := p.Stat(ctx, "docs/a.txt")
		Expect(err).NotTo(HaveOccurred())
		Expect(meta.Key).To(Equal("docs/a.txt"))
		Expect(meta.Size).To(Equal(int64(5)))
		Expect(meta.LastModified).To(BeNumerically(">", 0))

		By("treating equivalent spellings of a key as one object")
		put(ctx, p, "docs//./b.txt", "b")
		rc, err = p.Get(ctx, "docs/b.txt")
		Expect(err).NotTo(HaveOccurred())
		Expect(readAll(rc)).To(Equal([]byte("b")))

		By("reporting the key as the caller spelled it")
		meta, err = p.Stat(ctx, "docs//a.txt")
		Expect(err).NotTo(HaveOccurred())
		Expect(meta.Key).To(Equal("docs//a.txt"))

		By("listing a prefix as a path, sorted, with metadata")
		put(ctx, p, "docsx/c.txt", "c")
		put(ctx, p, "docs/sub/d.txt", "dd")
		Expect(listKeys(ctx, p, "docs")).To(Equal([]string{"docs/a.txt", "docs/b.txt", "docs/sub/d.txt"}))
		Expect(listKeys(ctx, p, "docs/")).To(Equal([]string{"docs/a.txt", "docs/b.txt", "docs/sub/d.txt"}))
		Expect(listKeys(ctx, p, "docs/a.txt")).To(Equal([]string{"docs/a.txt"}))
		Expect(listKeys(ctx, p, "")).To(Equal([]string{"docs/a.txt", "docs/b.txt", "docs/sub/d.txt", "docsx/c.txt"}))
		metas, err := p.List(ctx, "docs/sub")
		Expect(err).NotTo(HaveOccurred())
		Expect(metas).To(HaveLen(1))
		Expect(metas[0].Size).To(Equal(int64(2)))
		Expect(metas[0].LastModified).To(BeNumerically(">", 0))

		By("ordering a directory's entries before its siblings that sort between its name and its children")
		for _, key := range []string{"ord/a0", "ord/a.txt", "ord/a-b", "ord/a/x", "ord/a/b-c", "ord/a/b/z"} {
			put(ctx, p, key, "o")
		}
		Expect(listKeys(ctx, p, "ord")).To(Equal([]string{
			"ord/a/b/z", "ord/a/b-c", "ord/a/x", "ord/a-b", "ord/a.txt", "ord/a0",
		}))

		By("deleting idempotently")
		Expect(p.Delete(ctx, "docs/a.txt")).To(Succeed())
		Expect(p.Delete(ctx, "docs/a.txt")).To(Succeed())
		_, err = p.Get(ctx, "docs/a.txt")
		Expect(err).To(MatchError(storage.ErrNotFound))

		By("refusing every call after Close")
		Expect(p.Close()).To(Succeed())
		Expect(p.Close()).To(Succeed())
		_, err = p.Get(ctx, "docs/b.txt")
		Expect(err).To(MatchError(storage.ErrProviderClosed))
		Expect(p.Put(ctx, "docs/z", strings.NewReader("z"))).To(MatchError(storage.ErrProviderClosed))
		Expect(p.Delete(ctx, "docs/b.txt")).To(MatchError(storage.ErrProviderClosed))
		_, err = p.List(ctx, "")
		Expect(err).To(MatchError(storage.ErrProviderClosed))
		_, err = p.Exists(ctx, "docs/b.txt")
		Expect(err).To(MatchError(storage.ErrProviderClosed))
		_, err = p.Stat(ctx, "docs/b.txt")
		Expect(err).To(MatchError(storage.ErrProviderClosed))
	},
	Entry("Local", func() storage.Provider {
		p, err := storage.NewLocal(GinkgoT().TempDir(), "tenant-a")
		Expect(err).NotTo(HaveOccurred())
		return p
	}),
	Entry("Fake", func() storage.Provider { return &storagetest.Fake{} }),
)

var _ = Describe("Fake", func() {
	var (
		ctx  context.Context
		fake *storagetest.Fake
	)

	BeforeEach(func() {
		ctx = context.Background()
		fake = &storagetest.Fake{}
	})

	Describe("key validation", func() {
		It("rejects invalid keys with storage.ErrInvalidKey and stores nothing", func() {
			for _, key := range invalidKeys {
				Expect(fake.Put(ctx, key, strings.NewReader("x"))).To(MatchError(storage.ErrInvalidKey), "Put %q", key)
				_, err := fake.Get(ctx, key)
				Expect(err).To(MatchError(storage.ErrInvalidKey), "Get %q", key)
				Expect(fake.Delete(ctx, key)).To(MatchError(storage.ErrInvalidKey), "Delete %q", key)
				_, err = fake.Exists(ctx, key)
				Expect(err).To(MatchError(storage.ErrInvalidKey), "Exists %q", key)
				_, err = fake.Stat(ctx, key)
				Expect(err).To(MatchError(storage.ErrInvalidKey), "Stat %q", key)
			}
			for _, prefix := range invalidKeys[1:] {
				_, err := fake.List(ctx, prefix)
				Expect(err).To(MatchError(storage.ErrInvalidKey), "List %q", prefix)
			}
			Expect(fake.Objects()).To(BeEmpty())
		})

		It("rejects an invalid key before a scripted Func runs", func() {
			called := false
			fake.PutFunc = func(context.Context, string, io.Reader) error { called = true; return nil }
			fake.GetFunc = func(context.Context, string) (io.ReadCloser, error) { called = true; return nil, nil }
			fake.DeleteFunc = func(context.Context, string) error { called = true; return nil }
			fake.ExistsFunc = func(context.Context, string) (bool, error) { called = true; return true, nil }
			fake.StatFunc = func(context.Context, string) (*storage.ObjectMetadata, error) { called = true; return nil, nil }
			fake.ListFunc = func(context.Context, string) ([]storage.ObjectMetadata, error) { called = true; return nil, nil }

			Expect(fake.Put(ctx, "../x", strings.NewReader("x"))).To(MatchError(storage.ErrInvalidKey))
			_, err := fake.Get(ctx, "../x")
			Expect(err).To(MatchError(storage.ErrInvalidKey))
			Expect(fake.Delete(ctx, "../x")).To(MatchError(storage.ErrInvalidKey))
			_, err = fake.Exists(ctx, "../x")
			Expect(err).To(MatchError(storage.ErrInvalidKey))
			_, err = fake.Stat(ctx, "../x")
			Expect(err).To(MatchError(storage.ErrInvalidKey))
			_, err = fake.List(ctx, "../x")
			Expect(err).To(MatchError(storage.ErrInvalidKey))
			Expect(called).To(BeFalse())
		})
	})

	Describe("Put", func() {
		It("rejects a nil reader and stores nothing", func() {
			err := fake.Put(ctx, "a", nil)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("nil reader"))
			Expect(fake.Objects()).To(BeEmpty())
		})

		It("stores nothing when the reader fails", func() {
			put(ctx, fake, "a", "before")
			err := fake.Put(ctx, "a", io.MultiReader(strings.NewReader("part"), failingReader{}))
			Expect(err).To(MatchError(errScripted))
			Expect(fake.Objects()).To(Equal(map[string][]byte{"a": []byte("before")}))
		})

		It("stores nothing after Close", func() {
			Expect(fake.Close()).To(Succeed())
			Expect(fake.Put(ctx, "a", strings.NewReader("x"))).To(MatchError(storage.ErrProviderClosed))
			Expect(fake.Objects()).To(BeEmpty())
		})
	})

	Describe("scripted Funcs", func() {
		It("lets PutFunc decide alone and store nothing", func() {
			fake.PutFunc = func(context.Context, string, io.Reader) error { return errScripted }
			Expect(fake.Put(ctx, "a", strings.NewReader("x"))).To(MatchError(errScripted))
			Expect(fake.Objects()).To(BeEmpty())
		})

		It("lets GetFunc win over stored data", func() {
			put(ctx, fake, "a", "stored")
			fake.GetFunc = func(context.Context, string) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader("scripted")), nil
			}
			rc, err := fake.Get(ctx, "a")
			Expect(err).NotTo(HaveOccurred())
			Expect(readAll(rc)).To(Equal([]byte("scripted")))
		})

		It("lets GetFunc fail an otherwise present key", func() {
			put(ctx, fake, "a", "stored")
			fake.GetFunc = func(context.Context, string) (io.ReadCloser, error) { return nil, errScripted }
			_, err := fake.Get(ctx, "a")
			Expect(err).To(MatchError(errScripted))
		})

		It("lets DeleteFunc win and keep the object", func() {
			put(ctx, fake, "a", "stored")
			fake.DeleteFunc = func(context.Context, string) error { return errScripted }
			Expect(fake.Delete(ctx, "a")).To(MatchError(errScripted))
			Expect(fake.Objects()).To(HaveKey("a"))
		})

		It("lets ListFunc, ExistsFunc and StatFunc win", func() {
			fake.ListFunc = func(_ context.Context, prefix string) ([]storage.ObjectMetadata, error) {
				return []storage.ObjectMetadata{{Key: prefix + "/x", Size: 7}}, nil
			}
			fake.ExistsFunc = func(context.Context, string) (bool, error) { return true, nil }
			fake.StatFunc = func(_ context.Context, key string) (*storage.ObjectMetadata, error) {
				return &storage.ObjectMetadata{Key: key, Size: 9}, nil
			}
			metas, err := fake.List(ctx, "p")
			Expect(err).NotTo(HaveOccurred())
			Expect(metas).To(Equal([]storage.ObjectMetadata{{Key: "p/x", Size: 7}}))
			ok, err := fake.Exists(ctx, "nothing")
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			meta, err := fake.Stat(ctx, "nothing")
			Expect(err).NotTo(HaveOccurred())
			Expect(meta.Size).To(Equal(int64(9)))
		})

		It("refuses scripted calls after Close", func() {
			fake.GetFunc = func(context.Context, string) (io.ReadCloser, error) { return nil, errScripted }
			Expect(fake.Close()).To(Succeed())
			_, err := fake.Get(ctx, "a")
			Expect(err).To(MatchError(storage.ErrProviderClosed))
		})
	})

	Describe("Close", func() {
		It("returns CloseErr and still closes the provider", func() {
			fake.CloseErr = errScripted
			Expect(fake.Close()).To(MatchError(errScripted))
			_, err := fake.Exists(ctx, "a")
			Expect(err).To(MatchError(storage.ErrProviderClosed))
		})
	})

	Describe("copies", func() {
		It("does not alias the caller's buffer, Get data or Objects", func() {
			buf := []byte("original")
			Expect(fake.Put(ctx, "a", bytes.NewReader(buf))).To(Succeed())
			buf[0] = 'X'

			rc, err := fake.Get(ctx, "a")
			Expect(err).NotTo(HaveOccurred())
			got := readAll(rc)
			Expect(got).To(Equal([]byte("original")))
			got[0] = 'Y'

			objects := fake.Objects()
			Expect(objects["a"]).To(Equal([]byte("original")))
			objects["a"][0] = 'Z'
			delete(objects, "a")

			Expect(fake.Objects()).To(Equal(map[string][]byte{"a": []byte("original")}))
		})

		It("gives List and Stat metadata that cannot alias stored state", func() {
			put(ctx, fake, "a", "x")
			metas, err := fake.List(ctx, "")
			Expect(err).NotTo(HaveOccurred())
			metas[0].Size = 99
			meta, err := fake.Stat(ctx, "a")
			Expect(err).NotTo(HaveOccurred())
			Expect(meta.Size).To(Equal(int64(1)))
			meta.Size = 99
			meta, err = fake.Stat(ctx, "a")
			Expect(err).NotTo(HaveOccurred())
			Expect(meta.Size).To(Equal(int64(1)))
		})
	})

	It("is safe for concurrent use", func() {
		var wg sync.WaitGroup
		for i := range 20 {
			wg.Add(1)
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				key := "k/" + string(rune('a'+i))
				Expect(fake.Put(ctx, key, strings.NewReader("v"))).To(Succeed())
				_, err := fake.Exists(ctx, key)
				Expect(err).NotTo(HaveOccurred())
				_, err = fake.Stat(ctx, key)
				Expect(err).NotTo(HaveOccurred())
				_, err = fake.List(ctx, "k")
				Expect(err).NotTo(HaveOccurred())
				_ = fake.Objects()
				if i%2 == 0 {
					Expect(fake.Delete(ctx, key)).To(Succeed())
				}
			}()
		}
		wg.Wait()
		Expect(fake.Objects()).To(HaveLen(10))
	})
})

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errScripted }
