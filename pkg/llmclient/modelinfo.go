package llmclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
)

// maxModelCatalogSize bounds the model catalog body (4 MiB). A catalog entry
// is a few hundred bytes, so this is far above any real gateway.
const maxModelCatalogSize = 4 * 1024 * 1024

// maxModelCatalogEntries bounds how many entries ParseModelCatalog keeps.
const maxModelCatalogEntries = 10000

// ErrModelCatalogUnsupported reports a gateway that does not publish a model
// catalog (HTTP 404 or 405 at /v1/model/info), so the model behind an alias
// cannot be read from it.
var ErrModelCatalogUnsupported = errors.New("the LLM gateway does not publish a model catalog")

// ErrInvalidModelCatalog reports a model catalog that is not the expected
// JSON shape.
var ErrInvalidModelCatalog = errors.New("invalid LLM gateway model catalog")

// ModelDeployment is one deployment a gateway serves: the alias clients call
// it by, the gateway's ID for it, and the upstream model it forwards to.
// Nothing else the gateway reports about a deployment (its base URL, keys,
// extra parameters) is read or kept.
type ModelDeployment struct {
	// Alias is the gateway's model_name.
	Alias string
	// ID is the deployment ID (LiteLLM's model_info.id), the value of the
	// x-litellm-model-id header a completion carries.
	ID string
	// Model is the upstream model (LiteLLM's litellm_params.model), such as
	// "openai/qwen3.8-flash-next".
	Model string
}

// ModelCatalog is implemented by a client that can read the gateway's model
// catalog, which names the upstream model behind each alias.
type ModelCatalog interface {
	// ModelCatalog returns the gateway's deployments. It returns
	// ErrModelCatalogUnsupported when the gateway publishes none,
	// ErrAuthentication when the key may not read it, ErrInvalidModelCatalog
	// when the response is not a catalog, and an error wrapping
	// ErrGatewayUnavailable for a network failure or a 5xx.
	ModelCatalog(ctx context.Context) ([]ModelDeployment, error)
}

var _ ModelCatalog = (*client)(nil)

// ParseModelCatalog reads a LiteLLM /v1/model/info body:
// {"data":[{"model_name":..,"litellm_params":{"model":..},"model_info":{"id":..}}]}.
// The body is untrusted: unknown fields are ignored, an entry whose fields
// have the wrong type is skipped, and at most maxModelCatalogEntries entries
// are kept. A body that is not an object with a "data" array is an error
// wrapping ErrInvalidModelCatalog.
func ParseModelCatalog(body []byte) ([]ModelDeployment, error) {
	var envelope struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidModelCatalog, err)
	}
	if envelope.Data == nil {
		return nil, fmt.Errorf("%w: no \"data\" array", ErrInvalidModelCatalog)
	}
	out := make([]ModelDeployment, 0, min(len(envelope.Data), maxModelCatalogEntries))
	for _, raw := range envelope.Data {
		if len(out) == maxModelCatalogEntries {
			break
		}
		var entry struct {
			ModelName     string `json:"model_name"`
			LiteLLMParams struct {
				Model string `json:"model"`
			} `json:"litellm_params"`
			ModelInfo struct {
				ID string `json:"id"`
			} `json:"model_info"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			continue
		}
		out = append(out, ModelDeployment{Alias: entry.ModelName, ID: entry.ModelInfo.ID, Model: entry.LiteLLMParams.Model})
	}
	return out, nil
}

// ModelCatalog implements ModelCatalog with GET <gateway>/v1/model/info,
// authenticated like a completion. It makes one attempt: the caller decides
// whether a failure is worth a retry.
func (c *client) ModelCatalog(ctx context.Context) (deployments []ModelDeployment, err error) {
	ctx, span := c.startSpan(ctx, "llmclient.ModelCatalog")
	defer span.End()
	defer func() {
		if c.modelCatalogCounter != nil {
			c.modelCatalogCounter.Add(ctx, 1, metric.WithAttributes(attribute.Bool("success", err == nil)))
		}
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "model catalog read failed")
			c.recordErrorMetric(ctx)
			return
		}
		span.SetAttributes(attribute.Int("catalog.entries", len(deployments)))
		span.SetStatus(codes.Ok, "")
	}()

	url := strings.TrimRight(c.cfg.GatewayURL, "/") + "/v1/model/info"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("creating model catalog request: %w", err)
	}
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("reading the LLM gateway model catalog: %w: %w", err, ErrGatewayUnavailable)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		return nil, fmt.Errorf("GET /v1/model/info returned HTTP %d: %w", resp.StatusCode, ErrModelCatalogUnsupported)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("GET /v1/model/info returned HTTP %d: %w", resp.StatusCode, ErrAuthentication)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, fmt.Errorf("GET /v1/model/info returned HTTP %d: %w", resp.StatusCode, ErrGatewayUnavailable)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelCatalogSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading the model catalog body: %w: %w", err, ErrGatewayUnavailable)
	}
	if len(body) > maxModelCatalogSize {
		return nil, fmt.Errorf("model catalog exceeds %d bytes: %w", maxModelCatalogSize, ErrResponseTooLarge)
	}
	return ParseModelCatalog(body)
}
