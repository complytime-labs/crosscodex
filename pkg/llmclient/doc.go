// Package llmclient provides a provider-agnostic LLM gateway client for
// chat completions and embeddings using the OpenAI-compatible API.
//
// The client handles:
//   - Chat completions via /v1/chat/completions (OpenAI, Ollama, vLLM, LiteLLM)
//   - Text embeddings via /v1/embeddings
//   - Credential resolution from environment variables, files, or vault URIs
//   - Exponential backoff retry on rate limits (429) and server errors (5xx)
//   - Gateway mode: skip client-side retry when an upstream gateway handles retries
//   - Tenant isolation with tenant ID validation on every request
//   - Model allow-list enforcement
//   - OTel tracing and metrics via WithTelemetry option
//   - Audit event emission via AuditEmitter interface
//
// Gateway mode:
//
// When GatewayMode is true in the config, the client makes exactly one attempt
// per request. This is intended for deployments where an upstream proxy (e.g.,
// LiteLLM, Kong, Portkey) handles retry, failover, and circuit breaking. All
// other client behavior (telemetry, audit, tenant validation, model allow-list)
// is unchanged.
//
// Example usage:
//
//	client, err := llmclient.NewClient(cfg.LLM,
//	    llmclient.WithTelemetry(tracerProvider, meterProvider),
//	    llmclient.WithAuditEmitter(auditEmitter),
//	)
//	if err != nil {
//	    return err
//	}
//	defer client.Close()
//
//	resp, err := client.Complete(ctx, &llmclient.CompletionRequest{
//	    Model:    "compliance-judge",
//	    Messages: []llmclient.ChatMessage{
//	        {Role: llmclient.RoleSystem, Content: "You are a compliance analyst."},
//	        {Role: llmclient.RoleUser, Content: "Summarize NIST SP 800-53 AC-2."},
//	    },
//	    TenantID: "acme-corp",
//	    JobID:    "job-12345",
//	})
//
// Credential references use URI schemes:
//
//	api_key_ref: "env:LLM_API_KEY"        # Environment variable
//	api_key_ref: "file:/run/secrets/key"   # File (must be mode 0600)
//	api_key_ref: "vault:secret/llm/key"    # Vault (not yet implemented)
package llmclient
