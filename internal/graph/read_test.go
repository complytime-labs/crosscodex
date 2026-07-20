package graph

import (
	"errors"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
	"google.golang.org/grpc/codes"
)

func TestMapGraphError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected codes.Code
	}{
		{"nil", nil, codes.OK},
		{"NodeNotFound", graphdb.ErrNodeNotFound, codes.NotFound},
		{"EdgeNotFound", graphdb.ErrEdgeNotFound, codes.NotFound},
		{"GraphNotFound", graphdb.ErrGraphNotFound, codes.NotFound},
		{"InvalidCypher", graphdb.ErrInvalidCypher, codes.InvalidArgument},
		{"TenantRequired", graphdb.ErrTenantRequired, codes.InvalidArgument},
		{"ReadOnlyViolation", graphdb.ErrReadOnlyViolation, codes.PermissionDenied},
		{"unknown", errors.New("unknown"), codes.Internal},
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
		expected codes.Code
	}{
		{"nil", nil, codes.OK},
		{"NotFound", vectordb.ErrNotFound, codes.NotFound},
		{"ModelNotFound", vectordb.ErrModelNotFound, codes.NotFound},
		{"InvalidDimension", vectordb.ErrInvalidDimension, codes.InvalidArgument},
		{"unknown", errors.New("unknown"), codes.Internal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mapVectorError(tt.err); got != tt.expected {
				t.Errorf("mapVectorError(%v) = %v, want %v", tt.err, got, tt.expected)
			}
		})
	}
}
