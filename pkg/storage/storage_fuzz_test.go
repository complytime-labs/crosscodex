package storage_test

import (
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/storage"
)

func FuzzValidateKey(f *testing.F) {
	f.Add("valid/path/to/file.json")
	f.Add("")
	f.Add("/absolute/path")
	f.Add("../parent/escape")
	f.Add("path/../traversal")
	f.Add("path/./current")
	f.Add("back\\slash")
	f.Add(string([]byte{0x00}))
	f.Add("very/deep/path/that/is/quite/long/and/has/many/segments")
	f.Add("unicode/\u6587\u4EF6/path")

	f.Fuzz(func(t *testing.T, key string) {
		// Must not panic regardless of input
		_ = storage.ExportValidateKey(key)
	})
}
