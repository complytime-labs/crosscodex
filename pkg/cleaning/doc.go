// Package cleaning finds suspect mapping verdicts in the graph and retires or
// restores them under policy or a reviewer's decision.
//
// The building blocks are pure or narrow:
//   - StageRecord records which prompt and models produced each job stage,
//     and PromptStale and ModelStale decide whether a stage is stale;
//   - BuildPairs and Detect turn the verdicts crosswalk.LoadPair reads into
//     findings, one per raised signal;
//   - BuildPlan compares findings with the stored ones under the tenant's
//     policy and hashes the result into a plan token; StateToken does the
//     same for one stored finding;
//   - FindingStore is the authoritative record of findings and their
//     append-only decisions;
//   - RetireEdges and RestoreEdges are the only graph mutations: retire
//     supersedes live edges, restore recreates retired ones under derived
//     IDs.
//
// Engine composes them. Preview plans a run and writes nothing. Apply
// recomputes the plan, refuses it unless its token is the previewed one, runs
// every pre-check (signer, retire cap, live target edges) before writing,
// records the findings and one policy decision per retired finding under one
// signed in-toto link, then applies the retirements to the graph. Decide
// records a reviewer's retire, dismiss or restore against the finding's
// state token, signed with its own link, and applies it. Graph changes are
// recorded before they are applied; while any is outstanding, Preview, Apply
// and Decide are refused and Resume finishes them. Each change is timed by
// its decision, so a resumed one repeats exactly.
//
// WithEngineTelemetry adds a span per public method, the counter
// cleaning.findings.total, the gauge cleaning.findings.pending and the
// histogram cleaning.operation.duration_ms. The histogram's count by
// operation and outcome serves as the operations counter, so there is no
// separate one.
//
// The package never calls an LLM. It only records, compares and decides;
// callers own every model invocation.
package cleaning
