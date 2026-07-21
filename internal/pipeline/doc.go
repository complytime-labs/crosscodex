// Package pipeline implements the analysis pipeline orchestration layer.
//
// The Service type is the primary entry point. It implements
// PipelineServiceServer (gRPC) and gateway.PipelineBackend (REST),
// managing job lifecycle through analysis, synthesis, graph
// materialization, and attestation phases.
//
// # Job Lifecycle
//
// CreateJob persists a job and its stages to PostgreSQL, then spawns
// a goroutine that executes the full lifecycle synchronously:
//
//  1. Analysis: Build DAG from analyzer.Registry, execute via analysis.Engine
//  2. Synthesis: Rank and assess analysis outputs
//  3. Graph: Publish events for graph materialization
//  4. Attestation: Generate in-toto layout, collect links, verify chain
//
// Analysis stages are discovered dynamically from the DAG. The pipeline
// adds "synthesis" and "graph" as meta-stages. No analyzer knowledge
// is hardcoded.
//
// # Resume and Retry
//
// On startup, Start() queries for jobs with status "running" and resumes
// from the last incomplete stage. RetryJob resets failed stages and
// re-enters executeJob.
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
