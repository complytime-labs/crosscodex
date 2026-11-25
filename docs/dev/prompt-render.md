# Prompt rendering

How `crosscodex prompt render` shows the prompt a job sent. Usage and the caveats a user needs are in the README ("Seeing a prompt"). The implementation is `internal/pipeline/render_prompt.go` (`ServeRenderPrompt`), reached through `pipeline.RenderPrompt`, `gateway.RenderPrompt` and, in embedded mode, `cmd/crosscodex/embedded.go`.

## Why it reuses the analyzers' own code

A hand-written copy of a prompt drifts from the real one, and a drifted preview is worse than none: it reports a prompt no model saw. So the render path builds the `relationship`, `requires` or `critique` analyzer exactly as `NewProductionRegistry` does, from the same prompt registry and `config.AnalysisConfig`, and calls the `Render` method that the analyzer's `GenerateWork` renders every task with. The pair's parameters come from `pairAnalyzerParameters`, the function the job's own stage uses. Each of the three analyzers exposes that `Render` method for this purpose, so a change to a prompt changes the render with no second edit.

The analyzers get an LLM client that fails every call (`noLLM`, `ErrRenderCallsNoLLM`), so rendering cannot reach a model even if an analyzer is changed to call one.

## What the render depends on

- **Current configuration, not the job's.** The registry and `AnalysisConfig` are the running ones, wired by `WithPromptRegistry` and `WithAnalysisConfig`. A change to the templates or settings after a job ran shows in a later render of that job. The CLI help and README say so.
- **Controls** come from `composeControls`, the one place that builds the text analyzers see, through `CatalogControlsReader` (the same path as `catalog controls`). A render never re-derives control text.
- **With `job_id`:** the job's catalogs, the catalog names recorded when it was created, and its stored classify result (types and levels). **Without:** the request's catalogs, their current names, and no type or level, which is where a render can differ from a job's prompt.
- **Critique** reviews a proposed verdict: the request's, or the one `NewCritiqueVerdictProvider` reads from the job's stored results, as the job's critique stage reads it. A pair the job accepted as both a relationship and a REQUIRES, or as neither, is refused with `FailedPrecondition` naming the `--verdict` to use instead, because the stage reviewed each kind with its own prompt and one render shows one.

## Access control

The gateway cannot check job ownership before the call: an unknown job, another tenant's job and an unknown control must all answer one `NotFound`, and a prior `GetJob` would answer an unknown job with its own. So `gateway.RenderPrompt` calls the pipeline first and then, for a request with `job_id` whose answer was not `NotFound`, calls `authorizeJobOwner` (owner or admin, as for `GetJob`). Any other answer about a job, a render or a refusal (which can reveal a job's catalogs, verdicts or whether it ran) reaches only the owner or an admin; anyone else gets `PermissionDenied`. A render from catalogs alone needs no ownership, as reading the catalogs does not.

## Bounds and untrusted text

- `maxRenderIDBytes` (256), `maxRenderJustifications` (64) and `maxRenderJustificationBytes` (`MaxStoredJustificationBytes`) are security ceilings on one request, not tuning. A real job's justifications number at most its proposing panel's size.
- Control text and justifications are untrusted. The CLI prints messages verbatim between role fences (text) or JSON-escaped (`--json`); neither is safe as markup. `catalog controls` strips control and invisible format characters in text output only.
- `RenderPromptSources` fails closed: any nil field refuses every request. `pipeline.WithAnalysisConfig` is required for that reason.

## Embedded mode

`localPipelineBackend` (`cmd/crosscodex/embedded.go`) serves `RenderPrompt` and `ListCatalogControls` from the local database through the same `ServeRenderPrompt`; rendering needs no daemon. It builds the prompt registry on the first render, not at startup, because only `prompt render` reads the prompt layers: a layer that does not load must not stop every embedded command. A load failure is `FailedPrecondition`, naming `prompt.layer_paths` and `prompt.layers`.
