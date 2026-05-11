package graphdb

import "errors"

var (
	// ErrNodeNotFound indicates the specified node does not exist.
	ErrNodeNotFound = errors.New("node not found")

	// ErrEdgeNotFound indicates the specified edge does not exist.
	ErrEdgeNotFound = errors.New("edge not found")

	// ErrInvalidCypher indicates the openCypher query is malformed.
	ErrInvalidCypher = errors.New("invalid openCypher query")

	// ErrGraphNotFound indicates the specified graph does not exist.
	ErrGraphNotFound = errors.New("graph not found")
)
