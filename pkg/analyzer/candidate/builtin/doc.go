// Package builtin provides built-in candidate generators for the requires analyzer.
//
// The builtin generators implement different strategies for producing candidate
// control pairs for prerequisite analysis:
//
//   - SemanticGenerator: Selects top-K most similar targets based on embedding similarity
//   - KeywordGenerator: Identifies foundational targets by keyword matching
//   - LevelGenerator: Pairs sources with higher-level targets (Strategic→Tactical→Operational)
//
// All generators implement the candidate.Generator interface and can be configured
// via YAML parameters.
//
// Example usage:
//
//	semantic := builtin.NewSemanticGenerator()
//	keyword := builtin.NewKeywordGenerator()
//	level := builtin.NewLevelGenerator()
//
//	req := candidate.GenerateRequest{
//	    SourceControls: sources,
//	    TargetControls: targets,
//	    EmbeddingMatrix: matrix,
//	    Parameters: map[string]interface{}{
//	        "top_k": 50,
//	        "min_similarity": 60.0,
//	        "weight": 1.0,
//	    },
//	}
//
//	candidates, err := semantic.Generate(ctx, req)
package builtin
