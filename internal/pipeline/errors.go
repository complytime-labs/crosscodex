package pipeline

import "errors"

var ErrNotFound = errors.New("pipeline: not found")

var ErrInvalidJobID = errors.New("pipeline: invalid job ID")
