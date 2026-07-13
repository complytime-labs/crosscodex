package worker

import "errors"

// ErrInvalidMessage indicates a NATS message is missing required headers
// (X-Task-Id or X-Task-Type).
var ErrInvalidMessage = errors.New("worker: invalid message")

// ErrUnsupportedTaskType indicates the task type is not handled by this worker.
var ErrUnsupportedTaskType = errors.New("worker: unsupported task type")

// ErrInvalidPayload indicates the message payload could not be deserialized
// as a protobuf Struct.
var ErrInvalidPayload = errors.New("worker: invalid payload")

// ErrLLMCall indicates the upstream LLM gateway returned an error.
var ErrLLMCall = errors.New("worker: LLM call failed")

// ErrTenantConfig indicates the tenant's LLM configuration could not be
// resolved from the global config.
var ErrTenantConfig = errors.New("worker: tenant config resolution failed")

// ErrNotStarted indicates an operation was attempted on a worker that has
// not been started.
var ErrNotStarted = errors.New("worker: not started")

// ErrAlreadyStarted indicates Start was called on a worker that is already
// running.
var ErrAlreadyStarted = errors.New("worker: already started")
