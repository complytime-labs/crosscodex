package analyzer

import "github.com/complytime-labs/crosscodex/pkg/llmclient"

// MessagesForPayload converts chat messages into a structpb-compatible
// []interface{} for inclusion in task payloads. Each message becomes a
// map[string]interface{} with "role" and "content" keys.
func MessagesForPayload(messages []llmclient.ChatMessage) []interface{} {
	result := make([]interface{}, len(messages))
	for i, m := range messages {
		result[i] = map[string]interface{}{
			"role":    m.Role,
			"content": m.Content,
		}
	}
	return result
}
