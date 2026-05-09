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
)
