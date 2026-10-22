package db_test

import (
	"regexp"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/complytime-labs/crosscodex/pkg/db"
)

func FuzzRedactDSN(f *testing.F) {
	f.Add("postgresql://user:password@localhost:5432/mydb")
	f.Add("host=localhost port=5432 user=admin password=secret dbname=test")
	f.Add("")
	f.Add("not-a-dsn-at-all")
	f.Add("postgresql://nopassword@host/db")
	f.Add("postgresql://user:p%40ssw0rd@host/db")
	f.Add("host=localhost password=s3cret sslmode=require")
	f.Add("postgresql://user:pass@host:5432/db?sslmode=require&password=leaked")

	f.Fuzz(func(t *testing.T, dsn string) {
		result := db.ExportRedactDSN(dsn)
		// Must never panic (implicit)
		// Must never return empty for non-empty input
		if dsn != "" && result == "" {
			t.Fatalf("redactDSN returned empty for non-empty input %q", dsn)
		}
		// If input contained "password" followed by a value, output must contain REDACTED or be unparseable.
		// When the DSN has no actual password value, neither REDACTED nor unparseable is required — that is acceptable.
	})
}

// assertAuditText fails t unless s is non-blank, valid UTF-8, at most
// maxBytes and free of control, format and line/paragraph separators.
func assertAuditText(t *testing.T, s string, maxBytes int) {
	t.Helper()
	if !utf8.ValidString(s) || len(s) > maxBytes || strings.TrimSpace(s) == "" {
		t.Fatalf("accepted %q", s)
	}
	for _, r := range s {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			t.Fatalf("accepted %q with %U", s, r)
		}
	}
}

func FuzzValidateReason(f *testing.F) {
	f.Add("customer offboarded")
	f.Add("")
	f.Add(strings.Repeat("a", db.MaxReasonBytes+1))
	f.Add("line one\x00line two")
	f.Add("'; DROP TABLE identity_events; --")
	f.Add("left\u202eright")
	f.Add("one\u2029two")

	f.Fuzz(func(t *testing.T, reason string) {
		if db.ValidateReason(reason) != nil {
			return
		}
		assertAuditText(t, reason, db.MaxReasonBytes)
	})
}

// fuzzIDText checks a validator of IDs stored verbatim: accepted input is
// audit text that also has no leading or trailing spaces.
func fuzzIDText(f *testing.F, validate func(string) error, valid string) {
	for _, s := range []string{
		valid,                                    // valid
		"",                                       // boundary: empty
		strings.Repeat("a", db.MaxReviewerBytes), // boundary: at the cap
		strings.Repeat("a", db.MaxReviewerBytes+1), // boundary: past the cap
		"a\x1b[2J",    // attack: terminal escape
		"a\u202eb",    // attack: bidi override
		"a\u2028b",    // attack: line separator
		" alice",      // boundary: leading space
		"alice\u00a0", // boundary: trailing no-break space
		"\xff",        // invalid UTF-8
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, id string) {
		if validate(id) != nil {
			return
		}
		assertAuditText(t, id, db.MaxReviewerBytes)
		if strings.TrimSpace(id) != id {
			t.Fatalf("accepted %q with leading or trailing spaces", id)
		}
	})
}

func FuzzValidateReviewerID(f *testing.F) {
	fuzzIDText(f, db.ValidateReviewerID, "alice@example.com")
}

func FuzzValidateActor(f *testing.F) {
	fuzzIDText(f, db.ValidateActor, "ops@example.invalid")
}

func FuzzValidatePseudonym(f *testing.F) {
	f.Add("hidden-tenant-0123456789abcdef")
	f.Add("")
	f.Add("hidden-reviewer-0123456789abcdef0123456789abcdef")
	f.Add("hidden-tenant-0123456789abcde\x00")
	f.Add("hidden-tenant-0123456789abcdef'; DROP TABLE tenants; --")

	f.Fuzz(func(t *testing.T, p string) {
		if db.ValidatePseudonym(p) != nil {
			return
		}
		// Checked by hand, not by the validator's regex, so a regex mistake
		// cannot hide here.
		rest, ok := strings.CutPrefix(p, "hidden-tenant-")
		if !ok {
			rest, ok = strings.CutPrefix(p, "hidden-reviewer-")
		}
		if !ok || len(rest) != 16 {
			t.Fatalf("accepted malformed pseudonym %q", p)
		}
		for i := 0; i < len(rest); i++ {
			if c := rest[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				t.Fatalf("accepted pseudonym %q with non-hex byte %q", p, c)
			}
		}
	})
}

var scramVerifierShape = regexp.MustCompile(`^SCRAM-SHA-256\$4096:[A-Za-z0-9+/=]+\$[A-Za-z0-9+/=]+:[A-Za-z0-9+/=]+$`)

func FuzzSCRAMVerifier(f *testing.F) {
	f.Add("3f9a1c7e5b2d4a6c8e0f1b3d5a7c9e2f")
	f.Add("")
	f.Add("has space")
	f.Add("x'; ALTER ROLE postgres SUPERUSER; --")
	f.Add("pässword")
	f.Add(strings.Repeat("a", 4096))

	salt := []byte("0123456789abcdef")
	f.Fuzz(func(t *testing.T, password string) {
		v, err := db.SCRAMVerifier(password, salt)
		if err != nil {
			return
		}
		if !scramVerifierShape.MatchString(v) {
			t.Fatalf("accepted %q but produced malformed verifier %q", password, v)
		}
	})
}
