package natsbus

import "errors"

var (
	// ErrStreamNotFound indicates the stream does not exist.
	ErrStreamNotFound = errors.New("stream not found")

	// ErrConnectionClosed indicates the NATS connection is closed.
	ErrConnectionClosed = errors.New("connection closed")

	// ErrPublishFailed indicates message publication failed.
	ErrPublishFailed = errors.New("failed to publish message")

	// ErrNotStored indicates a durable publish was not confirmed stored by
	// JetStream: no stream captures the subject, the acknowledgement failed
	// or timed out, or the publish ack timeout is misconfigured. Errors
	// wrapping it also wrap ErrPublishFailed. The message may or may not
	// have reached a stream; a caller that cannot lose it must retry or fail.
	ErrNotStored = errors.New("message not stored")

	// ErrSubscribeFailed indicates subscription creation failed.
	ErrSubscribeFailed = errors.New("failed to create subscription")

	// ErrInvalidSubject indicates a subject could not be constructed
	// due to invalid tenant ID, job ID, or edge ID.
	ErrInvalidSubject = errors.New("invalid subject")

	// ErrInvalidJobID indicates a job ID cannot be a subject token: it is
	// empty, contains a NATS subject delimiter, or is longer than
	// MaxJobIDLength. Errors wrapping it also wrap ErrInvalidSubject.
	ErrInvalidJobID = errors.New("invalid job ID")

	// ErrEmbeddedStart indicates the embedded NATS server failed to start.
	ErrEmbeddedStart = errors.New("failed to start embedded NATS server")

	// ErrPermanent is returned (or wrapped) by a durable-consumer handler to say
	// the message can never be processed: it is terminated, not redelivered, and
	// in a work-queue stream it is removed. Any other handler error is retried
	// after ConsumerConfig.RedeliverDelay, up to MaxDeliver.
	ErrPermanent = errors.New("message cannot be processed; not redelivered")

	// ErrStreamDrift indicates a live JetStream stream differs from the
	// configuration this process would create it with. Startup refuses it
	// rather than overwrite a stream another role or an operator owns.
	ErrStreamDrift = errors.New("live stream differs from configuration")

	// ErrStreamCreate indicates a JetStream stream could not be created or updated.
	ErrStreamCreate = errors.New("failed to create stream")

	// ErrMissingProvenance indicates a received message is missing one or more
	// mandatory provenance headers. The error message lists the missing fields.
	ErrMissingProvenance = errors.New("missing provenance headers")

	// ErrContentHashMismatch indicates the received message payload does not
	// match the content hash in the X-Content-SHA256 provenance header.
	ErrContentHashMismatch = errors.New("content hash mismatch")

	// ErrExternalURLRequired means an operation needs an external NATS server
	// but nats.url is empty (embedded mode).
	ErrExternalURLRequired = errors.New("nats.url is empty (embedded mode)")
)
