package attestation

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"

	in_toto "github.com/in-toto/in-toto-golang/in_toto"
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
