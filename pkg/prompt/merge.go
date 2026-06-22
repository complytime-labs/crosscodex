package prompt

import (
	"fmt"

	"dario.cat/mergo"
)

// mergeSpecs deep-merges overlay onto a copy of base using the given slice strategy.
// Returns a new PromptSpec without modifying either input.
// sliceStrategy must be "replace", "append", or "deep_copy".
// Returns ErrLayerConflict if base and overlay have different names
// (both non-empty).
func mergeSpecs(base, overlay *PromptSpec, sliceStrategy string) (*PromptSpec, error) {
	if base.Name != "" && overlay.Name != "" && base.Name != overlay.Name {
		return nil, fmt.Errorf("cannot merge prompt %q with %q: %w",
			base.Name, overlay.Name, ErrLayerConflict)
	}

	// Work on a deep copy of base to avoid mutating inputs.
	result := copySpec(base)

	// Build mergo options based on slice strategy.
	opts := []func(*mergo.Config){
		mergo.WithOverride,
	}

	switch sliceStrategy {
	case "append":
		opts = append(opts, mergo.WithAppendSlice)
	case "deep_copy":
		opts = append(opts, mergo.WithSliceDeepCopy)
	case "replace", "":
		// Default: higher layer's slices replace lower layer's.
		// mergo.WithOverride handles this for non-empty slices.
	}

	if err := mergo.Merge(&result, overlay, opts...); err != nil {
		return nil, fmt.Errorf("merging prompt specs: %w", err)
	}

	return &result, nil
}

// copySpec creates a deep copy of a PromptSpec.
func copySpec(spec *PromptSpec) PromptSpec {
	cp := *spec

	// Deep copy slices.
	if spec.FewShot != nil {
		cp.FewShot = make([]FewShotExample, len(spec.FewShot))
		copy(cp.FewShot, spec.FewShot)
	}

	// Deep copy maps.
	if spec.Metadata != nil {
		cp.Metadata = make(map[string]string, len(spec.Metadata))
		for k, v := range spec.Metadata {
			cp.Metadata[k] = v
		}
	}

	return cp
}
