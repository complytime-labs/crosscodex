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
  probe_timeout: 0
  model_revisions: {}
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
  service_identities: []
database:
  dsn: ""
  migration_dsn: ""
  graph_dsn: ""
  tenant_admin_dsn: ""
  extensions: []
  max_conns: 10
  ssl_mode: prefer
nats:
  url: ""
  cluster: ""
  tls: false
  publish_ack_timeout: 10s
  materialize:
    max_deliver: 5
    ack_wait: 10m
    redeliver_delay: 30s
    barrier_poll_interval: 2s
  embedded:
    store_dir: ""
  streams:
    audit_llm_retention: 2160h
    audit_events_retention: 720h
    materialize_retention: 168h
server:
  addr: ":50051"
  workers: 4
  max_upload_size: 134217728
cli:
  output: table
  no_color: false
  endpoint: ""
  upload_chunk_size: 1048576
logging:
  level: warn
  format: text
tenants:
  enabled: false
  default_tenant: ""
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
  requirement_part_names: [statement]
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
    print_artifact_patterns: []
attestation:
  enabled: true
  private_key_path: ""
  public_key_path: ""
  expiry_duration: 8760h
  include_byproducts: true
  storage_retries: 3
  storage_retry_delay: 2s
prompt:
  capture_content: true
  allow_commands: false
  layer_paths: []
  layers:
    enabled: true
    order: []
analysis:
  engine:
    task_timeout: 5m
    success_timeout: 10m
    progress_flush_interval: 10s
    max_retries: 3
    retry_backoff: 1s
    retry_on_timeout: false
  classification:
    enabled: true
    model: ""
    max_text_length: 2000
    temperature: 0.0
    max_tokens: 20
  embedding:
    enabled: true
    models:
      - snowflake-arctic-embed2
    max_chars: 1500
    batch_size: 50
  relationship:
    enabled: false
    models: []
    max_source_chars: 1500
    max_target_chars: 800
    max_tokens: 300
    samples_per_model: 1
    sampling_temperature: 0.3
    actionable_types:
      - EQUIVALENT
      - SUPERSET_OF
      - SUBSET_OF
      - CONTRIBUTES_TO
      - COMPLEMENTS
      - CONFLICTS_WITH
    confidence_weights:
      high: 1.0
      medium: 0.67
      low: 0.33
    allow_shared_panel_models: false
  requires:
    enabled: false
    models: []
    samples_per_model: 1
    allow_even_samples: false
    sampling_temperature: 0.3
    max_tokens: 300
    consensus_threshold: 0.66
    max_error_rate: 0.34
    max_source_chars: 1500
    max_target_chars: 800
    allow_shared_panel_models: false
  critique:
    enabled: true
    models:
      - panel-1
      - panel-2
      - panel-3
    samples_per_model: 1
    allow_even_samples: false
    sampling_temperature: 0.3
    max_tokens: 300
    consensus_threshold: 0.66
    max_error_rate: 0.34
    max_source_chars: 1500
    max_target_chars: 800
    allow_shared_panel_models: false
    veto: false
  candidates:
    min_embedding_coverage: 0.8
    embed_model: snowflake-arctic-embed2
    max_per_source: 10
  artifact_adjudication:
    enabled: false
    models: []
    samples_per_model: 3
    allow_even_samples: false
    sampling_temperature: 0.3
    max_tokens: 200
    consensus_threshold: 0.66
    max_error_rate: 0.34
    max_attempts: 5
    lease_duration: 15m
    lease_batch: 10
    panel_concurrency: 4
    retry_base_delay: 1m
    retry_max_delay: 24h
  result_cache:
    enabled: true
    lookup_batch_size: 1000
worker:
  queue_group: "llm-workers"
  max_abandoned_collections: 10000
  max_in_flight: 1
synthesis:
  confidence_threshold: 0.5
  max_mappings_per_control: 10
  viability:
    type_mismatch_factor: 0.8
    skip_level_factor: 0.7
    integral_to_factor: 1.1
  assessment:
    iqr_good: 20.0
    iqr_poor: 10.0
    no_rel_high: 0.97
    no_rel_low: 0.80
    contested_warn: 0.20
    actionable_warn: 0.30
feedback:
  min_agreement: 0.5
  low_confidence: 0.5
cleaning:
  max_retire_per_run: 1000
  signals:
    equivalence_fanout:
      action: report
    cross_job_contradiction:
      action: report
    low_agreement:
      action: report
      min_agreement_fraction: 0.5
    conflicts_with:
      action: report
    stale_prompt:
      action: report
    stale_model:
      action: report
pipeline:
  max_concurrent_jobs: 10
  stage_timeout: 24h
  graph_materialize_timeout: 30m
  addr: ""
  endpoint: ""
ingestion:
  address: ""
  poll_interval: 2s
  convert_timeout: 10m
  max_document_bytes: 52428800
graph:
  max_bulk_edges: 1000
  max_similarity_catalogs: 100
  max_similarity_limit: 1000
retention:
  defaults:
    attestation: "7y"
    job_results: "90d"
    catalogs: "indefinite"
    embeddings: "90d"
    audit_decisions: "indefinite"
    audit_llm: "90d"
    audit_events: "30d"
  scan_schedule: ""
  archive:
    backend: ""
    s3:
      bucket: ""
      storage_class: ""
    local:
      path: ""
  tenants: {}
backup:
  dsn: ""
  destination:
    backend: ""
    local:
      path: ""
    s3:
      bucket: ""
      region: ""
      endpoint: ""
      storage_class: ""
  max_age:
    postgres: 26h
    objects: 26h
    nats: 26h
role: all
health:
  addr: ":9091"
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

// DefaultStructuringConfig returns the compiled defaults of catalog.structuring,
// the values an operator who sets nothing gets. A caller that must know whether
// a setting was changed (catalog import refuses settings that no longer apply
// to schema 1.4 output) compares against it rather than re-typing the values.
func DefaultStructuringConfig() (StructuringConfig, error) {
	node, err := defaultNode()
	if err != nil {
		return StructuringConfig{}, err
	}
	var cfg Config
	if err := node.Decode(&cfg); err != nil {
		return StructuringConfig{}, fmt.Errorf("decoding compiled defaults: %w", err)
	}
	return cfg.Catalog.Structuring, nil
}
