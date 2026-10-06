package tenant_test

import (
	"strings"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

func FuzzValidateTenantID(f *testing.F) {
	f.Add("acme-corp")
	f.Add("a")
	f.Add("")
	f.Add("UPPERCASE")
	f.Add("has spaces")
	f.Add("valid-tenant-123")
	f.Add("-starts-with-dash")
	f.Add("ends-with-dash-")
	f.Add("a--b")
	f.Add(string([]byte{0x00, 0x01, 0x02}))
	f.Add("a" + strings.Repeat("b", 50) + "c")
	f.Add("a" + strings.Repeat("b", 51) + "c")
	f.Add("x'); --")

	f.Fuzz(func(t *testing.T, id string) {
		if tenant.ValidateTenantID(id) != nil {
			return
		}
		// An accepted ID must keep the graph name crosscodex_<id> within
		// PostgreSQL's 63-byte identifier limit.
		if len(id) > 52 {
			t.Fatalf("accepted %d-byte tenant ID %q; the limit is 52", len(id), id)
		}
	})
}
