// Package llmclient provides an LLM gateway client for completions and embeddings.
//
// Handles API calls to the LLM gateway with rate limiting, retries, and
// tenant-scoped requests.
//
// Example usage:
//
//	client, err := llmclient.NewClient(cfg.LLM)
//	if err != nil {
//	    return err
//	}
//
//	resp, err := client.Complete(ctx, &llmclient.CompletionRequest{
//	    Prompt: "Summarize this NIST control",
//	    MaxTokens: 500,
//	})
package llmclient
