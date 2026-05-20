package graphdb

import "errors"

var (
	ErrNodeNotFound   = errors.New("node not found")
	ErrEdgeNotFound   = errors.New("edge not found")
	ErrInvalidCypher  = errors.New("invalid openCypher query")
	ErrGraphNotFound  = errors.New("graph not found")
	ErrTenantRequired = errors.New("tenant ID required")
)
