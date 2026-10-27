package analyzertest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/complytime-labs/crosscodex/pkg/analyzer"
)

// ErrUnscripted is wrapped by every error a stub returns for a call the test
// did not script. Match it with errors.Is; the message names the field to set.
var ErrUnscripted = errors.New("analyzertest: call not scripted")

func unscripted(method string) error {
	return fmt.Errorf("%w: %s called but Stub.%sFunc is not set; set it to script this call",
		ErrUnscripted, method, method)
}

var _ analyzer.Analyzer[proto.Message] = (*Stub[proto.Message])(nil)

// Stub stubs analyzer.Analyzer[T]. A set <Method>Func alone decides that
// method's result; an unset one returns ErrUnscripted. The zero value is
// ready to use. Set the exported fields before the first call; the methods
// are then safe for concurrent use.
type Stub[T proto.Message] struct {
	// AnalyzerName is returned by Name.
	AnalyzerName string
	// Dependencies is returned (as a copy) by DependsOn.
	Dependencies []string
	// Schema is returned by ResultSchema. Nil when unset.
	Schema proto.Message

	GenerateWorkFunc func(ctx context.Context, input T, cfg analyzer.AnalyzerConfig) ([]analyzer.Task, error)
	AggregateFunc    func(ctx context.Context, results []analyzer.TaskResult) (*analyzer.Output, error)

	mu     sync.Mutex
	inputs []T
}

func (s *Stub[T]) Name() string { return s.AnalyzerName }

func (s *Stub[T]) DependsOn() []string { return slices.Clone(s.Dependencies) }

func (s *Stub[T]) ResultSchema() proto.Message { return s.Schema }

func (s *Stub[T]) GenerateWork(ctx context.Context, input T, cfg analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
	s.mu.Lock()
	s.inputs = append(s.inputs, cloneInput(input))
	s.mu.Unlock()
	if s.GenerateWorkFunc == nil {
		return nil, unscripted("GenerateWork")
	}
	return s.GenerateWorkFunc(ctx, input, cfg)
}

func (s *Stub[T]) Aggregate(ctx context.Context, results []analyzer.TaskResult) (*analyzer.Output, error) {
	if s.AggregateFunc == nil {
		return nil, unscripted("Aggregate")
	}
	return s.AggregateFunc(ctx, results)
}

// Inputs returns a deep copy of every input GenerateWork received, in call
// order, including calls whose GenerateWorkFunc failed or was unset.
func (s *Stub[T]) Inputs() []T {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]T, len(s.inputs))
	for i, in := range s.inputs {
		out[i] = cloneInput(in)
	}
	return out
}

// cloneInput deep-copies a proto input. A nil input stays the zero value of T.
func cloneInput[T proto.Message](in T) T {
	c, _ := proto.Clone(in).(T)
	return c
}
