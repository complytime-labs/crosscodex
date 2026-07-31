package graph

import (
	"errors"
	"testing"

	"connectrpc.com/connect"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
)

func TestMapGraphError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected connect.Code
	}{
		{"nil", nil, connect.Code(0)},
		{"NodeNotFound", graphdb.ErrNodeNotFound, connect.CodeNotFound},
		{"EdgeNotFound", graphdb.ErrEdgeNotFound, connect.CodeNotFound},
		{"GraphNotFound", graphdb.ErrGraphNotFound, connect.CodeNotFound},
		{"InvalidCypher", graphdb.ErrInvalidCypher, connect.CodeInvalidArgument},
		{"TenantRequired", graphdb.ErrTenantRequired, connect.CodeInvalidArgument},
		{"ReadOnlyViolation", graphdb.ErrReadOnlyViolation, connect.CodePermissionDenied},
		{"unknown", errors.New("unknown"), connect.CodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mapGraphError(tt.err); got != tt.expected {
				t.Errorf("mapGraphError(%v) = %v, want %v", tt.err, got, tt.expected)
			}
		})
	}
}

func TestMapVectorError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected connect.Code
	}{
		{"nil", nil, connect.Code(0)},
		{"NotFound", vectordb.ErrNotFound, connect.CodeNotFound},
		{"ModelNotFound", vectordb.ErrModelNotFound, connect.CodeNotFound},
		{"InvalidDimension", vectordb.ErrInvalidDimension, connect.CodeInvalidArgument},
		{"unknown", errors.New("unknown"), connect.CodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mapVectorError(tt.err); got != tt.expected {
				t.Errorf("mapVectorError(%v) = %v, want %v", tt.err, got, tt.expected)
			}
		})
	}
}
