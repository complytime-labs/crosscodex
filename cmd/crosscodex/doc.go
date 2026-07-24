// Package main implements the crosscodex CLI binary.
//
// The CLI connects to crosscodexd via gRPC for compliance mapping operations.
// In single-node mode, it transparently starts an embedded daemon on first use.
package main
