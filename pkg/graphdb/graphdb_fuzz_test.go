package graphdb_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// FuzzCheckIdentifier checks that nothing CheckIdentifier accepts can carry
// Cypher or SQL syntax into the unquoted positions agedriver splices it into.
func FuzzCheckIdentifier(f *testing.F) {
	f.Add("Control")
	f.Add("MAPS_TO_2")
	f.Add("")
	f.Add("1Control")
	f.Add("Control) DETACH DELETE n //")
	f.Add("a$cypher$b")
	f.Add("Contrôle")

	f.Fuzz(func(t *testing.T, s string) {
		err := graphdb.CheckIdentifier("label", s)
		if err != nil {
			if !errors.Is(err, graphdb.ErrInvalidCypher) {
				t.Errorf("rejection of %q does not wrap ErrInvalidCypher: %v", s, err)
			}
			return
		}
		if s == "" || (s[0] >= '0' && s[0] <= '9') {
			t.Errorf("accepted %q, which is empty or starts with a digit", s)
		}
		if i := strings.IndexFunc(s, func(r rune) bool {
			return r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9')
		}); i >= 0 {
			t.Errorf("accepted %q, which has a non-identifier character at byte %d", s, i)
		}
	})
}

// FuzzDerivedID checks that every derived ID decodes back to the parts it was
// built from, which is what makes distinct part lists yield distinct IDs. The
// fuzzer supplies one string; splitting it on "|" yields the parts.
func FuzzDerivedID(f *testing.F) {
	f.Add("requires|job-9|ac-2|ac-1")
	f.Add("a_b|c")
	f.Add("a|b_c")
	f.Add("50%|x")
	f.Add("a%5Fb|c")
	f.Add("%25%5F__%|")
	f.Add("")

	decode := func(id string) []string {
		parts := strings.Split(id, "_")
		for i, p := range parts {
			parts[i] = strings.ReplaceAll(strings.ReplaceAll(p, "%5F", "_"), "%25", "%")
		}
		return parts
	}

	f.Fuzz(func(t *testing.T, joined string) {
		parts := strings.Split(joined, "|")
		id := graphdb.DerivedID(parts...)
		if got := decode(id); !slices.Equal(got, parts) {
			t.Errorf("DerivedID(%q) = %q decodes to %q", parts, id, got)
		}
	})
}

// FuzzCheckTenant checks that every tenant CheckTenant accepts is safe to
// splice into agedriver's quoted graph name and keeps that name within
// PostgreSQL's 63-byte identifier limit, and that every rejection wraps
// ErrTenantRequired.
func FuzzCheckTenant(f *testing.F) {
	f.Add("acme-corp")
	f.Add("t" + strings.Repeat("a", 50) + "1")
	f.Add("t" + strings.Repeat("a", 51) + "1")
	f.Add("")
	f.Add("Acme")
	f.Add("x'); --")
	f.Add("a$cypher$b")

	f.Fuzz(func(t *testing.T, id string) {
		err := graphdb.CheckTenant(id)
		if err != nil {
			if !errors.Is(err, graphdb.ErrTenantRequired) {
				t.Errorf("rejection of %q does not wrap ErrTenantRequired: %v", id, err)
			}
			if id != "" && !errors.Is(err, tenant.ErrInvalidTenant) {
				t.Errorf("rejection of %q does not wrap ErrInvalidTenant: %v", id, err)
			}
			return
		}
		if n := len("crosscodex_" + id); n > 63 {
			t.Errorf("accepted %q, whose %d-byte graph name PostgreSQL would truncate", id, n)
		}
		if i := strings.IndexFunc(id, func(r rune) bool {
			return r != '-' && (r < 'a' || r > 'z') && (r < '0' || r > '9')
		}); i >= 0 {
			t.Errorf("accepted %q, which has a character outside [a-z0-9-] at byte %d", id, i)
		}
	})
}

// FuzzCheckEdgeProperties checks that a property key CheckEdgeProperties
// accepts is an identifier and is never one the driver writes from an Edge
// field, so a caller cannot override Edge.ID or the temporal fields.
func FuzzCheckEdgeProperties(f *testing.F) {
	f.Add("kind")
	f.Add("id")
	f.Add("valid_from")
	f.Add("superseded_by")
	f.Add("has space")
	f.Add("id: 'x'}) DETACH DELETE e //")

	reserved := graphdb.ReservedEdgeKeys()
	f.Fuzz(func(t *testing.T, key string) {
		err := graphdb.CheckEdgeProperties(map[string]any{key: "v"})
		if err != nil {
			if !errors.Is(err, graphdb.ErrInvalidCypher) {
				t.Errorf("rejection of %q does not wrap ErrInvalidCypher: %v", key, err)
			}
			return
		}
		if _, ok := reserved[key]; ok {
			t.Errorf("accepted reserved key %q", key)
		}
		if graphdb.CheckIdentifier("property key", key) != nil {
			t.Errorf("accepted non-identifier key %q", key)
		}
	})
}
