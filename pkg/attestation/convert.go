package attestation

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strings"

	in_toto "github.com/in-toto/in-toto-golang/in_toto"
	"github.com/secure-systems-lab/go-securesystemslib/dsse"
	"github.com/secure-systems-lab/go-securesystemslib/signerverifier"
)

// artifactsToHashObj converts our Artifact slice to in-toto's material/product format.
func artifactsToHashObj(artifacts []Artifact) map[string]in_toto.HashObj {
	result := make(map[string]in_toto.HashObj, len(artifacts))
	for _, a := range artifacts {
		result[a.URI] = in_toto.HashObj{"sha256": a.Digest}
	}
	return result
}

// hashObjToArtifacts converts in-toto's material/product format back to our Artifact slice.
func hashObjToArtifacts(hashObjs map[string]in_toto.HashObj) []Artifact {
	artifacts := make([]Artifact, 0, len(hashObjs))
	for uri, hashes := range hashObjs {
		digest := hashes["sha256"]
		if digest == "" {
			for _, v := range hashes {
				digest = v
				break
			}
		}
		artifacts = append(artifacts, Artifact{URI: uri, Digest: digest})
	}
	return artifacts
}

// stepsToInToto converts our Step slice to in-toto Steps.
func stepsToInToto(steps []Step) []in_toto.Step {
	result := make([]in_toto.Step, len(steps))
	for i, s := range steps {
		expectedMats := make([][]string, len(s.ExpectedMaterials))
		for j, m := range s.ExpectedMaterials {
			expectedMats[j] = []string{"MATCH", m, "WITH", "PRODUCTS", "FROM", s.Name}
		}
		expectedProds := make([][]string, len(s.ExpectedProducts))
		for j, p := range s.ExpectedProducts {
			expectedProds[j] = []string{"MATCH", p, "WITH", "MATERIALS", "FROM", s.Name}
		}

		threshold := s.Threshold
		if threshold < 1 {
			threshold = 1
		}

		result[i] = in_toto.Step{
			Type:            "step",
			ExpectedCommand: s.Command,
			Threshold:       threshold,
			SupplyChainItem: in_toto.SupplyChainItem{
				Name:              s.Name,
				ExpectedMaterials: expectedMats,
				ExpectedProducts:  expectedProds,
			},
		}
	}
	return result
}

// inspectionsToInToto converts our Inspection slice to in-toto Inspections.
func inspectionsToInToto(inspections []Inspection) []in_toto.Inspection {
	result := make([]in_toto.Inspection, len(inspections))
	for i, insp := range inspections {
		result[i] = in_toto.Inspection{
			Type: "inspection",
			Run:  insp.Run,
			SupplyChainItem: in_toto.SupplyChainItem{
				Name: insp.Name,
			},
		}
	}
	return result
}

// signerToInTotoKey converts a crypto.Signer to an in-toto Key with public key only.
// Use this for verification keys and layout key maps.
func signerToInTotoKey(signer crypto.Signer, keyID string) (in_toto.Key, error) {
	pubDER, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return in_toto.Key{}, fmt.Errorf("marshal public key: %w", err)
	}

	pubPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubDER,
	})

	var key in_toto.Key
	if err := key.LoadKeyReaderDefaults(bytes.NewReader(pubPEM)); err != nil {
		return in_toto.Key{}, fmt.Errorf("load in-toto key: %w", err)
	}

	key.KeyID = keyID
	return key, nil
}

// signerToInTotoSigningKey converts a crypto.Signer to an in-toto Key that includes
// the private key material needed for signing operations.
func signerToInTotoSigningKey(signer crypto.Signer, keyID string) (in_toto.Key, error) {
	var privPEM []byte
	switch k := signer.(type) {
	case *ecdsa.PrivateKey:
		der, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			return in_toto.Key{}, fmt.Errorf("marshal EC private key: %w", err)
		}
		privPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	default:
		der, err := x509.MarshalPKCS8PrivateKey(signer)
		if err != nil {
			return in_toto.Key{}, fmt.Errorf("marshal private key: %w", err)
		}
		privPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	}

	var key in_toto.Key
	if err := key.LoadKeyReaderDefaults(bytes.NewReader(privPEM)); err != nil {
		return in_toto.Key{}, fmt.Errorf("load in-toto signing key: %w", err)
	}

	key.KeyID = keyID
	return key, nil
}

// inTotoStepsToSteps converts in-toto steps back to domain steps.
func inTotoStepsToSteps(steps []in_toto.Step) []Step {
	result := make([]Step, len(steps))
	for i, s := range steps {
		result[i] = Step{
			Name:      s.Name,
			Threshold: s.Threshold,
			Command:   s.ExpectedCommand,
		}
		// Extract material/product names from MATCH rules.
		// MATCH rules are ["MATCH", name, "WITH", ...] — name is at index 1.
		for _, m := range s.ExpectedMaterials {
			if len(m) > 1 {
				result[i].ExpectedMaterials = append(result[i].ExpectedMaterials, m[1])
			}
		}
		for _, p := range s.ExpectedProducts {
			if len(p) > 1 {
				result[i].ExpectedProducts = append(result[i].ExpectedProducts, p[1])
			}
		}
	}
	return result
}

// inTotoInspectionsToInspections converts in-toto inspections back to domain inspections.
func inTotoInspectionsToInspections(inspections []in_toto.Inspection) []Inspection {
	result := make([]Inspection, len(inspections))
	for i, insp := range inspections {
		result[i] = Inspection{
			Name: insp.Name,
			Run:  insp.Run,
		}
	}
	return result
}

// pubKeyToInTotoKey converts a crypto.PublicKey to an in-toto Key.
func pubKeyToInTotoKey(pub crypto.PublicKey, keyID string) (in_toto.Key, error) {
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return in_toto.Key{}, fmt.Errorf("marshal public key: %w", err)
	}

	pubPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubDER,
	})

	var key in_toto.Key
	if err := key.LoadKeyReaderDefaults(bytes.NewReader(pubPEM)); err != nil {
		return in_toto.Key{}, fmt.Errorf("load in-toto verification key: %w", err)
	}

	key.KeyID = keyID
	return key, nil
}

// parseDSSEEnvelope unmarshals raw bytes as a DSSE envelope.
func parseDSSEEnvelope(data []byte) (*dsse.Envelope, error) {
	var env dsse.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("unmarshal DSSE envelope: %w", err)
	}
	return &env, nil
}

// verifyDSSESignature verifies a DSSE envelope's signature using an in-toto Key.
// This operates at the DSSE level and does not decode the payload, avoiding
// canonical JSON parsing issues with raw newlines in string values.
func verifyDSSESignature(ctx context.Context, env *dsse.Envelope, key in_toto.Key) error {
	sslibKey := signerverifier.SSLibKey{
		KeyIDHashAlgorithms: key.KeyIDHashAlgorithms,
		KeyType:             key.KeyType,
		KeyVal: signerverifier.KeyVal{
			Public:  key.KeyVal.Public,
			Private: key.KeyVal.Private,
		},
		Scheme: key.Scheme,
		KeyID:  key.KeyID,
	}

	var verifier dsse.Verifier
	var err error
	switch sslibKey.KeyType {
	case signerverifier.ECDSAKeyType:
		verifier, err = signerverifier.NewECDSASignerVerifierFromSSLibKey(&sslibKey)
	case signerverifier.RSAKeyType:
		verifier, err = signerverifier.NewRSAPSSSignerVerifierFromSSLibKey(&sslibKey)
	case signerverifier.ED25519KeyType:
		verifier, err = signerverifier.NewED25519SignerVerifierFromSSLibKey(&sslibKey)
	default:
		return fmt.Errorf("unsupported key type %q", sslibKey.KeyType)
	}
	if err != nil {
		return fmt.Errorf("create verifier: %w", err)
	}

	ev, err := dsse.NewEnvelopeVerifier(verifier)
	if err != nil {
		return fmt.Errorf("create envelope verifier: %w", err)
	}

	_, err = ev.Verify(ctx, env)
	return err
}

// parseLayoutFromDSSE extracts an in-toto Layout from a DSSE envelope's payload.
// The payload is base64-encoded canonical JSON, which may contain raw newlines
// in string values (from PEM-encoded keys). This function fixes those newlines
// before parsing with the standard JSON decoder.
func parseLayoutFromDSSE(env *dsse.Envelope) (*in_toto.Layout, error) {
	payloadBytes, err := env.DecodeB64Payload()
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}

	fixed := fixCanonicalJSONNewlines(payloadBytes)

	var layout in_toto.Layout
	if err := json.Unmarshal(fixed, &layout); err != nil {
		return nil, fmt.Errorf("parse layout JSON: %w", err)
	}
	return &layout, nil
}

// fixCanonicalJSONNewlines escapes raw newlines inside JSON string values.
// Canonical JSON (RFC 8785 / cjson.EncodeCanonical) may produce unescaped
// newlines in string values (e.g., PEM-encoded keys), which Go's standard
// json.Unmarshal rejects. This function walks the JSON bytes and replaces
// raw \n inside quoted strings with the escaped sequence \\n.
func fixCanonicalJSONNewlines(data []byte) []byte {
	var result strings.Builder
	result.Grow(len(data))
	inString := false
	escaped := false

	for i := 0; i < len(data); i++ {
		b := data[i]
		if escaped {
			result.WriteByte(b)
			escaped = false
			continue
		}
		if b == '\\' && inString {
			result.WriteByte(b)
			escaped = true
			continue
		}
		if b == '"' {
			inString = !inString
			result.WriteByte(b)
			continue
		}
		if b == '\n' && inString {
			result.WriteString(`\n`)
			continue
		}
		result.WriteByte(b)
	}
	return []byte(result.String())
}
