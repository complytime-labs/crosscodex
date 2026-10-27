package rpcserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"go.opentelemetry.io/otel/trace"
)

// NewErrorReference returns a random 128-bit reference, hex-encoded, for an
// error whose detail is withheld from the client (CWE-209). The server logs
// the full error under the reference and the client message names it, so an
// operator can find the cause from what the user reports. Unlike a trace ID,
// the server always generates it, so a caller cannot choose or omit it.
func NewErrorReference() string {
	b := make([]byte, 16)
	// crypto/rand.Read never returns an error; it crashes the program
	// instead when the operating system cannot supply randomness.
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// LocalTraceID returns the trace ID of the span in ctx when it is valid and
// was started in this process, or "" otherwise. A remote span context holds
// the trace ID the caller sent, which the caller chooses, so it is not
// logged as this server's own.
func LocalTraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() || sc.IsRemote() {
		return ""
	}
	return sc.TraceID().String()
}
