package analyzertest

import (
	"slices"

	"google.golang.org/protobuf/proto"

	"github.com/complytime-labs/crosscodex/pkg/analyzer"
)

var (
	_ analyzer.Analyzer[proto.Message] = (*CacheableStub[proto.Message])(nil)
	_ analyzer.Cacheable               = (*CacheableStub[proto.Message])(nil)
)

// CacheableStub is a Stub that also opts in to result reuse
// (analyzer.Cacheable). Script GenerateWork and Aggregate through the
// embedded Stub's fields; set them on the CacheableStub (promoted fields),
// never by copying a Stub into it.
type CacheableStub[T proto.Message] struct {
	Stub[T]
	// Models is returned, as a copy, by CacheModels.
	Models []analyzer.ModelUse
	// CacheIdentityFunc, if set, alone decides CacheIdentity's result.
	// Unset, every task is uncacheable (ok=false): "not cached" is the
	// interface's own default, so there is no ErrUnscripted here.
	CacheIdentityFunc func(task analyzer.Task) (analyzer.CacheRequest, bool)
}

// CacheModels returns a copy of Models.
func (s *CacheableStub[T]) CacheModels() []analyzer.ModelUse { return slices.Clone(s.Models) }

// CacheIdentity returns CacheIdentityFunc's result, or ok=false when unset.
func (s *CacheableStub[T]) CacheIdentity(task analyzer.Task) (analyzer.CacheRequest, bool) {
	if s.CacheIdentityFunc == nil {
		return analyzer.CacheRequest{}, false
	}
	return s.CacheIdentityFunc(task)
}
