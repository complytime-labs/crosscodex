package tenant_test

import (
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

	f.Fuzz(func(t *testing.T, id string) {
		// Must not panic regardless of input
		_ = tenant.ValidateTenantID(id)
	})
}
