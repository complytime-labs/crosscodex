// Package analyzer provides the plugin interface for analysis capabilities.
//
// Analyzers extract compliance-relevant information from source code,
// infrastructure definitions, and other artifacts.
//
// Example usage:
//
//	registry := analyzer.NewRegistry()
//	err := registry.Register(terraformAnalyzer)
//	if err != nil {
//	    return err
//	}
//
//	analyzer, err := registry.Get("terraform")
//	if err != nil {
//	    return err
//	}
//
//	resp, err := analyzer.Analyze(ctx, &analyzer.AnalyzeRequest{
//	    ArtifactURI: "file:///path/to/main.tf",
//	})
package analyzer
