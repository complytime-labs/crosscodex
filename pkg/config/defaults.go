package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

const defaultConfigYAML = `
llm:
  gateway_url: ""
  gateway_mode: false
  default_model: ""
  embedding_model: ""
  api_key_ref: ""
  allowed_models: []
  max_retries: 3
  timeout: 30
storage:
  objects:
    backend: local
    base_path: ""
    bucket: ""
    region: ""
    endpoint: ""
tls:
  mode: "off"
  ca: ""
  cert: ""
  key: ""
  fips:
    enabled: false
  cipher_allow: []
  cipher_deny: []
database:
  dsn: ""
  graph_dsn: ""
  extensions: []
  max_conns: 10
  ssl_mode: prefer
nats:
  url: ""
  cluster: ""
  tls: false
  embedded:
    store_dir: ""
  streams:
    audit_llm_retention: 2160h
    audit_events_retention: 720h
server:
  grpc_addr: ":50051"
  http_addr: ":8080"
  workers: 4
cli:
  output: table
  no_color: false
  endpoint: ""
logging:
  level: info
  format: text
tenants:
  enabled: false
  default_tenant: ""
  allowed_tenants: []
auth:
  x509_mappings: []
observability:
  endpoint: ""
  protocol: grpc
  tracing:
    endpoint: ""
    protocol: ""
    sample_rate: 1.0
  metrics:
    endpoint: ""
    protocol: ""
    interval: 30s
catalog:
  structuring:
    section_pattern: ""
    decompose: true
    min_decompose_words: 40
    filter_by_keywords: false
    keywords: []
    chunk_chars: 3000
    max_validation_chars: 800
    allowed_formats: []
    max_heading_repeats: 3
attestation:
  enabled: true
  private_key_path: ""
  public_key_path: ""
  expiry_duration: 8760h
  include_byproducts: true
prompt:
  capture_content: true
  allow_commands: false
  layer_paths: []
  layers:
    enabled: true
    order: []
analysis:
  classification:
    enabled: true
    model: ""
    max_text_length: 2000
    temperature: 0.0
    max_tokens: 20
`

func defaultNode() (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(defaultConfigYAML), &doc); err != nil {
		return nil, fmt.Errorf("parsing compiled defaults: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("compiled defaults produced empty document")
	}
	return doc.Content[0], nil
}
