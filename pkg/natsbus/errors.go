package natsbus

import "errors"

var (
	// ErrStreamNotFound indicates the stream does not exist.
	ErrStreamNotFound = errors.New("stream not found")

	// ErrConnectionClosed indicates the NATS connection is closed.
	ErrConnectionClosed = errors.New("connection closed")

	// ErrPublishFailed indicates message publication failed.
	ErrPublishFailed = errors.New("failed to publish message")

	// ErrSubscribeFailed indicates subscription creation failed.
	ErrSubscribeFailed = errors.New("failed to create subscription")

	// ErrInvalidSubject indicates a subject could not be constructed
	// due to invalid tenant ID, job ID, or edge ID.
	ErrInvalidSubject = errors.New("invalid subject")

	// ErrEmbeddedStart indicates the embedded NATS server failed to start.
	ErrEmbeddedStart = errors.New("failed to start embedded NATS server")

	// ErrStreamCreate indicates a JetStream stream could not be created or updated.
	ErrStreamCreate = errors.New("failed to create stream")

	// ErrMissingProvenance indicates a received message is missing one or more
	// mandatory provenance headers. The error message lists the missing fields.
	ErrMissingProvenance = errors.New("missing provenance headers")

	// ErrContentHashMismatch indicates the received message payload does not
	// match the content hash in the X-Content-SHA256 provenance header.
	ErrContentHashMismatch = errors.New("content hash mismatch")
)
