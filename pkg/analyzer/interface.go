package analyzer

import "context"

// Analyzer is the plugin interface for analysis capabilities.
//
// Implementations must be safe for concurrent use.
type Analyzer interface {
	// Name returns the analyzer identifier.
	Name() string

	// Analyze processes an artifact and returns findings.
	Analyze(ctx context.Context, req *AnalyzeRequest) (*AnalyzeResponse, error)
}

// Registry manages analyzer plugins.
//
// Implementations must be safe for concurrent use.
type Registry interface {
	// Register adds an analyzer to the registry.
	// Returns ErrAlreadyRegistered if an analyzer with the same name exists.
	Register(a Analyzer) error

	// Get retrieves an analyzer by name.
	// Returns ErrNotFound if the analyzer does not exist.
	Get(name string) (Analyzer, error)

	// List returns the names of all registered analyzers.
	List() []string

	// Unregister removes an analyzer from the registry.
	Unregister(name string) error
}
