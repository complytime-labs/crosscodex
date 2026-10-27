package storagetest_test

import (
	"context"
	"io"
	"maps"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/storage"
	"github.com/complytime-labs/crosscodex/pkg/storage/storagetest"
)

// spellingGen draws an equivalent spelling of the cleaned key: doubled or
// dotted separators and a trailing slash all name the same object.
func spellingGen(key string) *rapid.Generator[string] {
	segs := strings.Split(key, "/")
	return rapid.Custom(func(t *rapid.T) string {
		sep := rapid.SampledFrom([]string{"/", "//", "/./"}).Draw(t, "sep")
		out := strings.Join(segs, sep)
		if rapid.Bool().Draw(t, "trailingSlash") {
			out += "/"
		}
		return out
	})
}

// keyGen draws keys from a small alphabet so operations collide often. Keys
// are single-segment or two-segment; the two shapes never collide as a
// file-versus-directory pair because first segments carry a "d" or "f" tag.
func keyGen() *rapid.Generator[string] {
	name := rapid.SampledFrom([]string{"a", "b", "c"})
	return rapid.Custom(func(t *rapid.T) string {
		if rapid.Bool().Draw(t, "nested") {
			return "d" + name.Draw(t, "dir") + "/" + name.Draw(t, "leaf")
		}
		return "f" + name.Draw(t, "file")
	})
}

var _ = Describe("Property Specifications", Ordered, func() {
	It("Get, Exists, Stat and List agree with a reference map and with storage.NewLocal", func() {
		type op struct {
			Put      bool
			Key      string // cleaned form, the reference map's key
			Spelling string // what the call passes
			Data     []byte
		}
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			ctx := context.Background()
			ops := rapid.SliceOf(rapid.Custom(func(t *rapid.T) op {
				key := keyGen().Draw(t, "key")
				return op{
					Put:      rapid.Bool().Draw(t, "put"),
					Key:      key,
					Spelling: spellingGen(key).Draw(t, "spelling"),
					Data:     rapid.SliceOf(rapid.Byte()).Draw(t, "data"),
				}
			})).Draw(t, "ops")

			fake := &storagetest.Fake{}
			local, err := storage.NewLocal(GinkgoT().TempDir(), "tenant-a")
			if err != nil {
				t.Fatalf("NewLocal: %v", err)
			}
			want := map[string][]byte{}
			for _, o := range ops {
				for _, p := range []storage.Provider{fake, local} {
					if o.Put {
						err = p.Put(ctx, o.Spelling, strings.NewReader(string(o.Data)))
					} else {
						err = p.Delete(ctx, o.Spelling)
					}
					if err != nil {
						t.Fatalf("op %+v: %v", o, err)
					}
				}
				if o.Put {
					want[o.Key] = o.Data
				} else {
					delete(want, o.Key)
				}
			}

			wantKeys := slices.Sorted(maps.Keys(want))
			for _, p := range []storage.Provider{fake, local} {
				metas, err := p.List(ctx, "")
				if err != nil {
					t.Fatalf("List: %v", err)
				}
				gotKeys := make([]string, 0, len(metas))
				for _, m := range metas {
					gotKeys = append(gotKeys, m.Key)
				}
				if !slices.Equal(gotKeys, wantKeys) {
					t.Fatalf("List = %v, want %v", gotKeys, wantKeys)
				}
			}
			for _, key := range []string{"fa", "fb", "fc", "da/a", "da/b", "db/c", "dc/a"} {
				data, present := want[key]
				for _, p := range []storage.Provider{fake, local} {
					ok, err := p.Exists(ctx, key)
					if err != nil || ok != present {
						t.Fatalf("Exists(%q) = %v, %v; want %v", key, ok, err, present)
					}
					meta, err := p.Stat(ctx, key)
					if present && (err != nil || meta.Size != int64(len(data))) {
						t.Fatalf("Stat(%q) = %+v, %v; want size %d", key, meta, err, len(data))
					}
					if !present && err == nil {
						t.Fatalf("Stat(%q) succeeded for absent key", key)
					}
					rc, err := p.Get(ctx, key)
					if present != (err == nil) {
						t.Fatalf("Get(%q) err = %v; present %v", key, err, present)
					}
					if err == nil {
						got, err := io.ReadAll(rc)
						if err != nil {
							t.Fatalf("reading Get(%q): %v", key, err)
						}
						if err := rc.Close(); err != nil {
							t.Fatalf("closing Get(%q): %v", key, err)
						}
						if string(got) != string(data) {
							t.Fatalf("Get(%q) = %q, want %q", key, got, data)
						}
					}
				}
			}
			if !maps.EqualFunc(fake.Objects(), want, slices.Equal[[]byte]) {
				t.Fatalf("Objects() = %v, want %v", fake.Objects(), want)
			}
		})
	})
})
