package backup_test

import (
	"bytes"
	"errors"
	"path"
	"strings"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/backup"
	"github.com/complytime-labs/crosscodex/pkg/storage"
)

func FuzzDecodeManifest(f *testing.F) {
	var valid bytes.Buffer
	if err := backup.Encode(&valid, sampleManifest()); err != nil {
		f.Fatalf("encode seed: %v", err)
	}
	v := valid.String()
	f.Add([]byte(v))                                                                // valid
	f.Add([]byte(`{}`))                                                             // invalid: empty
	f.Add([]byte(strings.Replace(v, `"id"`, `"x":1,"id"`, 1)))                      // unknown field
	f.Add([]byte(strings.Replace(v, "artifacts/a.json", "../../../etc/passwd", 1))) // attack: traversal
	f.Add([]byte(strings.Replace(v, "base_000000010000000000000004", "base_$(id)", 1)))
	f.Add([]byte(v + "{}"))                                                                // trailing data
	f.Add([]byte(strings.Replace(v, `"schema_version": 1`, `"schema_version": 1e309`, 1))) // boundary
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := backup.Decode(bytes.NewReader(data))
		if err != nil {
			if m != nil {
				t.Fatal("Decode returned a manifest with an error")
			}
			// A bytes.Reader never errors on read, so any rejection here
			// must come from validation and wrap ErrInvalidManifest.
			if !errors.Is(err, backup.ErrInvalidManifest) {
				t.Fatalf("rejected input without wrapping ErrInvalidManifest: %v", err)
			}
			return
		}
		for tenantID, entries := range m.Objects.Tenants {
			for _, e := range entries {
				if storage.ValidateKey(e.Key) != nil {
					t.Fatalf("accepted unsafe key %q for tenant %q", e.Key, tenantID)
				}
				if path.Clean(e.Key) != e.Key {
					t.Fatalf("accepted non-canonical key %q for tenant %q", e.Key, tenantID)
				}
			}
		}
		if backup.ValidatePointID(m.ID) != nil {
			t.Fatalf("accepted bad point ID %q", m.ID)
		}
		var buf bytes.Buffer
		if err := backup.Encode(&buf, m); err != nil {
			t.Fatalf("accepted manifest does not re-encode: %v", err)
		}
		if _, err := backup.Decode(&buf); err != nil {
			t.Fatalf("re-encoded manifest does not decode: %v", err)
		}
	})
}
