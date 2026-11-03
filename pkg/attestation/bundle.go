package attestation

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// bundleShape is the stored form of one attested run: the signed layout
// and one signed link per layout step, each kept as its exact envelope
// bytes so signatures still verify.
type bundleShape struct {
	Layout json.RawMessage   `json:"layout"`
	Links  []json.RawMessage `json:"links"`
}

// bundleHint tells a reader what ParseBundle accepts.
const bundleHint = `an attestation bundle is a JSON object with a "layout" envelope and a non-empty "links" list of envelopes`

// MarshalBundle encodes a signed layout and its links as an attestation
// bundle, the form ParseBundle reads. It refuses a nil layout or an empty
// link list (ErrInvalidBundle), which no complete run produces.
func MarshalBundle(layout *SignedLayout, links []*SignedLink) ([]byte, error) {
	if layout == nil || len(layout.Raw) == 0 {
		return nil, fmt.Errorf("encode bundle: no layout: %w", ErrInvalidBundle)
	}
	if len(links) == 0 {
		return nil, fmt.Errorf("encode bundle: no links: %w", ErrInvalidBundle)
	}
	b := bundleShape{Layout: json.RawMessage(layout.Raw), Links: make([]json.RawMessage, len(links))}
	for i, l := range links {
		if l == nil || len(l.Raw) == 0 {
			return nil, fmt.Errorf("encode bundle: link %d is empty: %w", i, ErrInvalidBundle)
		}
		b.Links[i] = json.RawMessage(l.Raw)
	}
	data, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("encode bundle: %w: %w", ErrInvalidBundle, err)
	}
	return data, nil
}

// ParseBundle decodes an attestation bundle written by MarshalBundle into
// its layout and links, without verifying them; pass the result to
// Generator.VerifyChain. Only the envelope bytes are set on the returned
// values, since VerifyChain reads everything else from them. Input that is
// not a bundle, including a single link or layout envelope, fails with
// ErrInvalidBundle and a description of the expected shape.
func ParseBundle(data []byte) (*SignedLayout, []*SignedLink, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, nil, fmt.Errorf("not JSON or not an object (%v): %s: %w", err, bundleHint, ErrInvalidBundle)
	}
	if _, ok := probe["payloadType"]; ok {
		return nil, nil, fmt.Errorf("this is a single signed envelope, not a bundle: %s: %w", bundleHint, ErrInvalidBundle)
	}
	var b bundleShape
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, nil, fmt.Errorf("decode bundle (%v): %s: %w", err, bundleHint, ErrInvalidBundle)
	}
	if isEmptyJSON(b.Layout) {
		return nil, nil, fmt.Errorf(`no "layout": %s: %w`, bundleHint, ErrInvalidBundle)
	}
	if len(b.Links) == 0 {
		return nil, nil, fmt.Errorf(`no "links": %s: %w`, bundleHint, ErrInvalidBundle)
	}
	links := make([]*SignedLink, len(b.Links))
	for i, raw := range b.Links {
		if isEmptyJSON(raw) {
			return nil, nil, fmt.Errorf(`"links"[%d] is empty: %s: %w`, i, bundleHint, ErrInvalidBundle)
		}
		links[i] = &SignedLink{Raw: []byte(raw)}
	}
	return &SignedLayout{Raw: []byte(b.Layout)}, links, nil
}

// isEmptyJSON reports whether raw is absent or the JSON null literal.
func isEmptyJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}
