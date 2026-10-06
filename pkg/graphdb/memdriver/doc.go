// Package memdriver implements graphdb.GraphDB in memory for tests and
// examples. It honors the contract documented on graphdb.GraphDB, enforced by
// internal/testspecs.GraphDBContractBehavior, except that ExecuteQuery returns
// graphdb.ErrNotSupported: memdriver has no openCypher engine.
//
// Create each tenant's graph with CreateGraph before using it, mirroring
// production, where a trigger creates the graph when the tenant is inserted.
//
// Properties are normalized on write to what Apache AGE returns on read
// (string, bool and float64 are stored as is; int, int64 and float32 become
// float64; every other type, including the other integer types and slices,
// becomes its %v string) and copied on read, so callers cannot mutate
// stored state.
//
// memdriver performs no I/O and carries no telemetry. Do not wire it into a
// production binary.
package memdriver
