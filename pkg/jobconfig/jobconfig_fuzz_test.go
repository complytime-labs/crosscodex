package jobconfig_test

import (
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/jobconfig"
)

func FuzzExtractCatalogs(f *testing.F) {
	for _, seed := range []string{
		`{"catalog_id":"cat-a","target_catalog_id":"cat-b","bidirectional":true}`, // valid crosswalk
		`{"catalog_id":"cat-a"}`, // valid: pre-target config
		`{"catalog_id":""}`,      // invalid: empty source
		`{"catalog_id":"cat-a","target_catalog_id":"cat-a","bidirectional":true}`, // boundary: bidirectional within one catalog
		`{"catalog_id":"cat-a","catalog_id":"cat-b"}`,                             // boundary: duplicate key
		`{"catalog_id":"a\u0000b","target_catalog_id":"\u202e"}`,                  // attack: NUL and bidi override
		`{"catalog_id":7}`, // invalid type
		`{"catalog_id":"cat-a","target_catalog_id":"cat-b","source_catalog":{"catalog_id":"cat-b","name":"x"}}`, // boundary: snapshot of the other side
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, stored []byte) {
		got, ok := jobconfig.ExtractCatalogs(stored)
		if !ok {
			return
		}
		if got.SourceID == "" || got.TargetID == "" {
			t.Fatalf("accepted %q with an empty catalog: %+v", stored, got)
		}
		if got.Bidirectional && !got.CrossCatalog() {
			t.Fatalf("accepted %q as bidirectional within one catalog: %+v", stored, got)
		}
		if !got.CrossCatalog() && got.TargetLabel != got.SourceLabel {
			t.Fatalf("accepted %q with two labels for one catalog: %+v", stored, got)
		}
	})
}
