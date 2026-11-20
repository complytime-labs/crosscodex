package connectutil_test

import (
	"slices"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/tenant/connectutil"
)

func FuzzParseUserRoles(f *testing.F) {
	f.Add("admin")
	f.Add("admin,reviewer")
	f.Add("")
	f.Add("admin,")
	f.Add("root")
	f.Add("admin\x00")

	f.Fuzz(func(t *testing.T, header string) {
		roles, err := connectutil.ParseUserRoles(header)
		if err != nil {
			return
		}
		if len(roles) == 0 {
			t.Fatalf("accepted %q as an empty role list", header)
		}
		for _, role := range roles {
			if !slices.Contains(authn.KnownRoles, role) {
				t.Fatalf("accepted %q with unknown role %q", header, role)
			}
		}
	})
}
