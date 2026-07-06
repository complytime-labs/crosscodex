// Package pipeline implements the analysis pipeline orchestration layer.
//
// The pipeline coordinates multiple analysis phases:
//   - classify: Framework-specific control classification
//   - embedding: Semantic similarity via vector embeddings
//   - candidates: Generate prerequisite candidate pairs
//   - requires: LLM panel voting on prerequisite relationships
//   - relationship: LLM panel voting on semantic relationships
//   - graph: Materialization to graph database
//
// # CandidateProvider Pattern
//
// The RequiresCandidateProvider bridges candidate generation (which writes
// to requires_candidates table) to the requires analyzer (which needs pairs
// to vote on). This separation allows candidate generation to run as a
// separate phase and persist results before expensive LLM voting begins.
//
// Usage in a pipeline orchestrator:
//
//	// Create database connection
//	dbPool := db.NewPool(cfg.Database)
//	tenantConn := db.NewTenantConnection(dbPool)
//
//	// Create candidate provider
//	candidateProvider := pipeline.NewRequiresCandidateProvider(
//		tenantConn,
//		pipeline.WithCandidateTelemetry(tracerProvider, meterProvider),
//	)
//
//	// Create requires analyzer with candidate provider
//	requiresAnalyzer := requires.New(
//		llmClient,
//		promptRegistry,
//		candidateProvider,
//		cfg.Requires,
//		requires.WithTelemetry(tracerProvider, meterProvider),
//	)
//
//	// Register analyzer
//	analyzer.Register(registry, requiresAnalyzer)
//
// The orchestrator would then:
//  1. Run candidate.Registry.Generate() to populate requires_candidates table
//  2. Call requiresAnalyzer.GenerateWork() which uses candidateProvider.Candidates()
//  3. Execute voting tasks via LLM workers
//  4. Aggregate results via requiresAnalyzer.Aggregate()
//  5. Compute consensus using consensus.Computer
//  6. Persist to requires_votes and requires_consensus tables
//  7. Create graph edges via graphdb.CreateRequiresEdge()
package pipeline
