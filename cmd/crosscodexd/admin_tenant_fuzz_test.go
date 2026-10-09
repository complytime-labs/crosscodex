package main

import (
	"bytes"
	"strings"
	"testing"
	"unicode"

	dbpkg "github.com/complytime-labs/crosscodex/pkg/db"
)

func FuzzParseTenantImport(f *testing.F) {
	f.Add([]byte("tenants:\n  - tenant_id: acme\n    display_name: Acme Corp\n"))                                        // valid
	f.Add([]byte(""))                                                                                                    // invalid: empty
	f.Add([]byte("tenants:\n  - tenant_id: acme\n    display_name: A\n  - tenant_id: acme\n    display_name: B\n"))      // invalid: duplicate IDs
	f.Add([]byte("tenants:\n  - tenant_id: acme\n    display_name: A\n    status: suspended\n"))                         // invalid: unknown field
	f.Add([]byte("tenants:\n  - tenant_id: Acme_Corp\n    display_name: A\n"))                                           // invalid: malformed ID
	f.Add([]byte("tenants:\n  - tenant_id: " + strings.Repeat("a", 52) + "\n    display_name: A\n"))                     // boundary: longest ID
	f.Add([]byte(strings.Repeat("[", 5000) + strings.Repeat("]", 5000)))                                                 // attack: deep nesting
	f.Add([]byte("a: &a [x,x,x,x,x,x,x,x,x]\nb: &b [*a,*a,*a,*a,*a,*a,*a,*a,*a]\nc: &c [*b,*b,*b,*b,*b,*b,*b,*b,*b]\n" + // attack: alias bomb
		"d: &d [*c,*c,*c,*c,*c,*c,*c,*c,*c]\ne: &e [*d,*d,*d,*d,*d,*d,*d,*d,*d]\ntenants: *e\n"))
	f.Add([]byte("tenants:\n  - tenant_id: acme\n    display_name: \"Acme\\x1b[2J\"\n")) // attack: terminal escape in name
	f.Fuzz(func(t *testing.T, data []byte) {
		specs, err := parseTenantImport(bytes.NewReader(data))
		if err != nil {
			if specs != nil {
				t.Fatal("parseTenantImport returned specs with an error")
			}
			return
		}
		if len(specs) == 0 {
			t.Fatal("accepted a file with no tenants")
		}
		seen := make(map[string]bool, len(specs))
		for _, s := range specs {
			if err := dbpkg.ValidateTenantSpec(s); err != nil {
				t.Fatalf("accepted an invalid spec %+v: %v", s, err)
			}
			if seen[s.ID] {
				t.Fatalf("accepted duplicate tenant ID %q", s.ID)
			}
			seen[s.ID] = true
			for _, r := range s.DisplayName {
				if unicode.IsControl(r) {
					t.Fatalf("accepted a display name with a control character: %q", s.DisplayName)
				}
			}
		}
	})
}
