package attestation_test

import (
	"encoding/json"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/attestation"
)

func FuzzFixCanonicalJSONNewlines(f *testing.F) {
	f.Add([]byte(`{"key": "value"}`))
	f.Add([]byte("{\"key\": \"value\\nwith\\nnewlines\"}"))
	f.Add([]byte(`{"key": "line1` + "\n" + `line2"}`))
	f.Add([]byte{})
	f.Add([]byte(`not json at all`))
	f.Add([]byte(`{"nested": {"a": "b\nc"}}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		result := attestation.FixCanonicalJSONNewlines(data)

		// If input is valid JSON, output must also be valid JSON.
		if json.Valid(data) {
			if !json.Valid(result) {
				t.Errorf("valid JSON input produced invalid JSON output:\n  input:  %q\n  output: %q", data, result)
			}
		}
	})
}

func FuzzParseDSSEEnvelope(f *testing.F) {
	f.Add([]byte(`{"payload":"dGVzdA==","payloadType":"application/vnd.in-toto+json","signatures":[{"keyid":"abc","sig":"AAAA"}]}`)) // DevSkim: ignore DS173237 — fuzz seed corpus, not a real key
	f.Add([]byte(`{}`))
	f.Add([]byte{})
	f.Add([]byte(`not json`))
	f.Add([]byte(`{"payload":"","payloadType":"","signatures":[]}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic regardless of input.
		_, _ = attestation.ParseDSSEEnvelope(data)
	})
}

func FuzzGenerateManifest(f *testing.F) {
	f.Add("file.txt", "abcdef1234567890")
	f.Add("", "")
	f.Add("path/to/file", "0000000000000000000000000000000000000000000000000000000000000000") // DevSkim: ignore DS173237 — fuzz seed corpus, SHA-256 zero digest not a secret
	f.Add("special chars !@#$%", "digest")
	f.Add(string([]byte{0x00, 0x01}), string([]byte{0xff, 0xfe}))

	f.Fuzz(func(t *testing.T, uri, digest string) {
		// Must not panic regardless of input.
		artifacts := []attestation.Artifact{{URI: uri, Digest: digest}}
		_ = attestation.GenerateManifest(artifacts)
	})
}

func FuzzParseBundle(f *testing.F) {
	f.Add([]byte(`{"layout":{"payloadType":"application/vnd.in-toto+json","payload":"e30=","signatures":[]},"links":[{"payloadType":"application/vnd.in-toto+json","payload":"e30=","signatures":[]}]}`))
	f.Add([]byte(`{"layout":{},"links":[]}`))
	f.Add([]byte(`{"payloadType":"application/vnd.in-toto+json","payload":"e30=","signatures":[]}`))
	f.Add([]byte(`{"layout":null,"links":[null]}`))
	f.Add([]byte(`{"layout":{},"links":[{}],"layout":"dup"}`))
	f.Add([]byte(`[]`))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		layout, links, err := attestation.ParseBundle(data)
		if err != nil {
			return
		}
		// An accepted bundle re-encodes to one that parses to the same envelopes.
		again, err := attestation.MarshalBundle(layout, links)
		if err != nil {
			t.Fatalf("accepted bundle does not re-encode: %v", err)
		}
		layout2, links2, err := attestation.ParseBundle(again)
		if err != nil {
			t.Fatalf("re-encoded bundle does not parse: %v", err)
		}
		if len(links2) != len(links) || len(links) == 0 {
			t.Fatalf("link count changed from %d to %d", len(links), len(links2))
		}
		if len(layout2.Raw) == 0 {
			t.Fatalf("re-encoded bundle lost its layout")
		}
	})
}
