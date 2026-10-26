// Package results reads a job's control mappings for the gateway: the
// synthesized mappings (after synthesis's confidence threshold and per-control
// cap) and the raw relationship analyzer output. Storage sits behind Reader so
// a non-PostgreSQL store replaces only PGStore; Service adapts Reader to the
// gateway's MappingsBackend.
package results
