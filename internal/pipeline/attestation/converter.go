// Package attestation provides the DAG-to-Layout bridge adapter for the pipeline service.
// It converts analyzer DAGs into attestation LayoutOptions without coupling pkg/attestation
// to pkg/analyzer.
package attestation

import (
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
)

// Converter transforms an analyzer DAG into attestation LayoutOptions.
type Converter struct{}

// NewConverter creates a new Converter.
func NewConverter() *Converter {
	return &Converter{}
}

// Convert maps a DAG to attestation LayoutOptions.
//
// Each analyzer becomes an attestation Step where:
//   - Step.Name = analyzer.Name()
//   - Step.ExpectedMaterials = product names of dependencies (from DependsOn())
//   - Step.ExpectedProducts = []string{analyzer.Name() + ".output"}
//   - Step.Command = []string{} (commands are runtime, not known at layout time)
//   - Step.Threshold = 1 (single signer required per step)
//
// Steps are ordered by DAG topological order. Level 0 steps have no ExpectedMaterials.
// The ExpiresIn field is NOT set — the caller provides it from AttestationConfig.ExpiryDuration.
func (c *Converter) Convert(dag *analyzer.DAG) attestation.LayoutOptions {
	analyzers := dag.Analyzers()
	steps := make([]attestation.Step, 0, len(analyzers))

	for _, a := range analyzers {
		var materials []string
		for _, dep := range a.DependsOn() {
			materials = append(materials, dep+".output")
		}

		step := attestation.Step{
			Name:              a.Name(),
			ExpectedMaterials: materials,
			ExpectedProducts:  []string{a.Name() + ".output"},
			Threshold:         1,
		}
		steps = append(steps, step)
	}

	return attestation.LayoutOptions{
		Steps: steps,
	}
}
