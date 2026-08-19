package pipeline

import (
	"fmt"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate/builtin"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

// BuildCandidateRegistry constructs a candidate.Registry with the builtin
// generators enabled in cfg.Generators registered. Unknown generator names
// are rejected rather than silently ignored, since a typo in config would
// otherwise silently disable a generator with no error anywhere.
func BuildCandidateRegistry(cfg config.CandidateConfig, opts ...candidate.RegistryOption) (*candidate.Registry, error) {
	reg := candidate.NewRegistry(opts...)
	for _, entry := range cfg.Generators {
		if !entry.Enabled {
			continue
		}
		var gen candidate.Generator
		switch entry.Name {
		case "keyword":
			gen = builtin.NewKeywordGenerator()
		case "level":
			gen = builtin.NewLevelGenerator()
		case "semantic":
			gen = builtin.NewSemanticGenerator()
		default:
			return nil, fmt.Errorf("BuildCandidateRegistry: unknown generator %q", entry.Name)
		}
		if err := reg.Register(gen); err != nil {
			return nil, fmt.Errorf("BuildCandidateRegistry: registering %q: %w", entry.Name, err)
		}
	}
	return reg, nil
}
