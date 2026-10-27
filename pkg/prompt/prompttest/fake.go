package prompttest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"

	"github.com/complytime-labs/crosscodex/pkg/prompt"
)

// ErrUnscripted is wrapped by every error a fake returns for a call the test
// did not script. Match it with errors.Is; the message names the field to set.
var ErrUnscripted = errors.New("prompttest: call not scripted")

func unscripted(method string) error {
	return fmt.Errorf("%w: %s called but Fake.%sFunc is not set; set it to script this call",
		ErrUnscripted, method, method)
}

var _ prompt.Registry = (*Fake)(nil)

// RenderCall is one recorded Render call.
type RenderCall struct {
	Name string
	Vars map[string]string
}

// Fake fakes prompt.Registry. A set <Method>Func alone decides that method's
// result; an unset one returns ErrUnscripted. The zero value is ready to use.
// Set the exported fields before the first call; the methods are then safe
// for concurrent use.
type Fake struct {
	// ResolveFunc, if set, alone decides what Resolve returns; unset, Resolve
	// returns ErrUnscripted.
	ResolveFunc func(ctx context.Context, name string) (*prompt.PromptSpec, error)
	// RenderFunc, if set, alone decides what Render returns. Every call is
	// recorded in Renders, whether or not RenderFunc is set.
	RenderFunc func(ctx context.Context, name string, vars map[string]string) (*prompt.ResolvedPrompt, error)
	// ListFunc, if set, alone decides what List returns; unset, List returns
	// ErrUnscripted.
	ListFunc func(ctx context.Context) ([]string, error)
	// LayersFunc, if set, alone decides what Layers returns; unset, Layers
	// returns ErrUnscripted.
	LayersFunc func(ctx context.Context, name string) ([]prompt.LayerInfo, error)

	mu      sync.Mutex
	renders []RenderCall
}

// Resolve returns what ResolveFunc decides, or an ErrUnscripted error when it
// is unset.
func (f *Fake) Resolve(ctx context.Context, name string) (*prompt.PromptSpec, error) {
	if f.ResolveFunc == nil {
		return nil, unscripted("Resolve")
	}
	return f.ResolveFunc(ctx, name)
}

// Render records the call, then returns what RenderFunc decides, or an
// ErrUnscripted error when it is unset.
func (f *Fake) Render(ctx context.Context, name string, vars map[string]string) (*prompt.ResolvedPrompt, error) {
	f.mu.Lock()
	f.renders = append(f.renders, RenderCall{Name: name, Vars: maps.Clone(vars)})
	f.mu.Unlock()
	if f.RenderFunc == nil {
		return nil, unscripted("Render")
	}
	return f.RenderFunc(ctx, name, vars)
}

// List returns what ListFunc decides, or an ErrUnscripted error when it is
// unset.
func (f *Fake) List(ctx context.Context) ([]string, error) {
	if f.ListFunc == nil {
		return nil, unscripted("List")
	}
	return f.ListFunc(ctx)
}

// Layers returns what LayersFunc decides, or an ErrUnscripted error when it
// is unset.
func (f *Fake) Layers(ctx context.Context, name string) ([]prompt.LayerInfo, error) {
	if f.LayersFunc == nil {
		return nil, unscripted("Layers")
	}
	return f.LayersFunc(ctx, name)
}

// Renders returns a deep copy of every Render call, in call order, including
// calls whose RenderFunc failed or was unset.
func (f *Fake) Renders() []RenderCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]RenderCall, len(f.renders))
	for i, c := range f.renders {
		out[i] = RenderCall{Name: c.Name, Vars: maps.Clone(c.Vars)}
	}
	return out
}
