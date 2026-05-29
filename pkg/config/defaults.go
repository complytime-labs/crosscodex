package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

const defaultConfigYAML = `
llm:
  gateway_url: ""
  default_model: ""
  embedding_model: ""
  api_key: ""
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
