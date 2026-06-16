package analyzer_test

import (
	"strings"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/analyzer"
)

func FuzzValidateName(f *testing.F) {
	f.Add("classify")
	f.Add("")
	f.Add("-starts-with-dash")
	f.Add("has_underscore")
	f.Add("has.dot")
	f.Add("a")
	f.Add("a" + strings.Repeat("b", 64))

	f.Fuzz(func(t *testing.T, name string) {
		// Must not panic regardless of input.
		_ = analyzer.ValidateName(name)
	})
}
