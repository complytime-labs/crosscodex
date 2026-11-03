package llmclient_test

import (
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/complytime-labs/crosscodex/pkg/llmclient"
)

func FuzzResolveCredential(f *testing.F) {
	// Valid scheme prefixes
	f.Add("env:MY_KEY")
	f.Add("file:/tmp/key")
	f.Add("vault:secret/data/key")

	// Empty and schemeless
	f.Add("")
	f.Add("no-scheme-at-all")

	// Unknown schemes
	f.Add("s3:bucket/key")
	f.Add("http://example.com")

	// Null bytes and binary
	f.Add(string([]byte{0x00}))
	f.Add("env:\x00BADVAR")
	f.Add("file:\x00/etc/passwd")

	// Edge cases
	f.Add("env:")
	f.Add("file:")
	f.Add("vault:")
	f.Add(":")
	f.Add(":value")

	f.Fuzz(func(t *testing.T, ref string) {
		// Must not panic regardless of input.
		_, _ = llmclient.ResolveCredential(ref)
	})
}

func FuzzParseRetryAfter(f *testing.F) {
	// Valid integer seconds
	f.Add("0")
	f.Add("1")
	f.Add("30")
	f.Add("120")

	// Negative
	f.Add("-1")
	f.Add("-999")

	// Non-numeric
	f.Add("")
	f.Add("abc")
	f.Add("not-a-number")

	// Very large
	f.Add("999999999999")
	f.Add("9223372036854775807") // max int64

	// Whitespace
	f.Add("  5  ")
	f.Add("\t10\n")

	// HTTP-date format
	f.Add("Fri, 31 Dec 1999 23:59:59 GMT")

	// Binary
	f.Add(string([]byte{0x00, 0x01, 0x02}))

	f.Fuzz(func(t *testing.T, header string) {
		// Must not panic regardless of input.
		d := llmclient.ParseRetryAfter(header)
		if d < 0 {
			t.Errorf("parseRetryAfter(%q) returned negative duration: %v", header, d)
		}
	})
}

func FuzzMessagesFromPayload(f *testing.F) {
	// Valid
	f.Add([]byte(`[{"role":"system","content":"s"},{"role":"user","content":"u"}]`))
	f.Add([]byte(`[{"role":"assistant","content":"a","extra":1}]`))

	// Invalid shape
	f.Add([]byte(`"not a list"`))
	f.Add([]byte(`[42, null, "x"]`))
	f.Add([]byte(`[{"content":"no role"}]`))

	// Boundary
	f.Add([]byte(`[]`))
	f.Add([]byte(`[{"role":"user"}]`))

	// Attack: role injection, case variants, non-string role
	f.Add([]byte(`[{"role":"tool","content":"x"}]`))
	f.Add([]byte(`[{"role":"SYSTEM","content":"x"}]`))
	f.Add([]byte(`[{"role":["system"],"content":"x"}]`))

	f.Fuzz(func(t *testing.T, data []byte) {
		v := &structpb.Value{}
		if err := v.UnmarshalJSON(data); err != nil {
			return
		}
		messages, err := llmclient.MessagesFromPayload(v)
		if err != nil {
			return
		}
		if len(messages) != len(v.GetListValue().Values) {
			t.Fatalf("decoded %d messages from %d items", len(messages), len(v.GetListValue().Values))
		}
		for i, m := range messages {
			if m.Role != llmclient.RoleSystem && m.Role != llmclient.RoleUser && m.Role != llmclient.RoleAssistant {
				t.Fatalf("messages[%d] accepted with role %q", i, m.Role)
			}
			if want := v.GetListValue().GetValues()[i].GetStructValue().GetFields()["content"].GetStringValue(); m.Content != want {
				t.Fatalf("messages[%d] content %q, want %q: the decoder must not alter content, which the cache hash covers", i, m.Content, want)
			}
		}
	})
}
