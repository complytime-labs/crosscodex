package graph

import (
	"errors"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/vectordb"
)

var _ = Describe("mapGraphError", func() {
	DescribeTable("maps graphdb errors to connect codes",
		func(err error, expected connect.Code) {
			Expect(mapGraphError(err)).To(Equal(expected))
		},
		Entry("nil", nil, connect.Code(0)),
		Entry("NodeNotFound", graphdb.ErrNodeNotFound, connect.CodeNotFound),
		Entry("EdgeNotFound", graphdb.ErrEdgeNotFound, connect.CodeNotFound),
		Entry("GraphNotFound", graphdb.ErrGraphNotFound, connect.CodeNotFound),
		Entry("InvalidCypher", graphdb.ErrInvalidCypher, connect.CodeInvalidArgument),
		Entry("TenantRequired", graphdb.ErrTenantRequired, connect.CodeInvalidArgument),
		Entry("ReadOnlyViolation", graphdb.ErrReadOnlyViolation, connect.CodePermissionDenied),
		Entry("NotSupported", graphdb.ErrNotSupported, connect.CodeUnimplemented),
		Entry("unknown", errors.New("unknown"), connect.CodeInternal),
	)
})

var _ = Describe("mapVectorError", func() {
	DescribeTable("maps vectordb errors to connect codes",
		func(err error, expected connect.Code) {
			Expect(mapVectorError(err)).To(Equal(expected))
		},
		Entry("nil", nil, connect.Code(0)),
		Entry("NotFound", vectordb.ErrNotFound, connect.CodeNotFound),
		Entry("ModelNotFound", vectordb.ErrModelNotFound, connect.CodeNotFound),
		Entry("InvalidDimension", vectordb.ErrInvalidDimension, connect.CodeInvalidArgument),
		Entry("unknown", errors.New("unknown"), connect.CodeInternal),
	)
})
