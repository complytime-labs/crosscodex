package attestation_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
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

// FuzzVerifyLink feeds Verify arbitrary envelopes. It must not panic, an
// accepted envelope must carry a signature naming the trusted key's ID, and
// ErrUntrustedKey must always come with ErrVerificationFailed.
func FuzzVerifyLink(f *testing.F) {
	kp, err := attestation.NewEphemeralKeyProvider()
	if err != nil {
		f.Fatalf("generate key: %v", err)
	}
	gen, err := attestation.NewGenerator(kp)
	if err != nil {
		f.Fatalf("create generator: %v", err)
	}
	keyID, err := kp.KeyID(context.Background())
	if err != nil {
		f.Fatalf("key ID: %v", err)
	}
	valid, err := gen.CreateLink(context.Background(), "step", []attestation.Artifact{{URI: "a", Digest: "aaa"}}, nil)
	if err != nil {
		f.Fatalf("create link: %v", err)
	}
	f.Add(valid.Raw)
	f.Add(bytes.Replace(valid.Raw, []byte(keyID), []byte(strings.Repeat("0", len(keyID))), 1))
	f.Add([]byte(`{"payload":"e30=","payloadType":"application/vnd.in-toto+json","signatures":[]}`))
	f.Add([]byte(`{"payload":"e30=","payloadType":"application/vnd.in-toto+json","signatures":[{"keyid":"` + keyID + `","sig":"AAAA"}]}`)) // DevSkim: ignore DS173237 — fuzz seed corpus, not a real key
	f.Add([]byte(`not json`))

	f.Fuzz(func(t *testing.T, data []byte) {
		_, err := gen.Verify(context.Background(), data)
		if errors.Is(err, attestation.ErrUntrustedKey) && !errors.Is(err, attestation.ErrVerificationFailed) {
			t.Fatalf("ErrUntrustedKey without ErrVerificationFailed: %v", err)
		}
		if untrusted, ok := errors.AsType[*attestation.UntrustedKeyError](err); ok && slices.Contains(untrusted.KeyIDs, keyID) {
			t.Fatalf("an untrusted-key error names the trusted key ID %s: %v", keyID, err)
		}
		if err != nil {
			return
		}
		var env struct {
			Signatures []struct {
				KeyID string `json:"keyid"`
			} `json:"signatures"`
		}
		if jerr := json.Unmarshal(data, &env); jerr != nil {
			t.Fatalf("accepted an envelope that is not JSON: %v", jerr)
		}
		for _, s := range env.Signatures {
			if s.KeyID == keyID {
				return
			}
		}
		t.Fatalf("accepted an envelope with no signature naming the trusted key %s", keyID)
	})
}
