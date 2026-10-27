// Package prompttest provides a test double for code that consumes
// prompt.Registry.
//
// # Fakes
//
// Fake fakes prompt.Registry. The real registry (prompt.NewRegistry) rejects
// nothing at its boundary: it accepts any name, including an empty one, and
// any (even nil) vars map, and answers an unknown name with
// prompt.ErrPromptNotFound and a template placeholder without a value with
// prompt.ErrMissingPlaceholder. Those answers come from layer resolution and
// template rendering, not from input checks, so a Fake does no validation of
// its own. A spec that needs either error scripts it, wrapping the exported
// sentinel.
//
// Gaps: a Fake does not layer, merge, cache or substitute anything, does not
// load few-shot sources and computes no ContentHash; whatever its Funcs
// return is what the caller sees. A Fake emits no telemetry.
//
// Fake follows the convention every shared fake in a pkg/<x>/<x>test package
// uses:
//
//   - Every method returns data, so each is scripted by a <Method>Func field
//     with the method's exact signature (Fake.ResolveFunc, Fake.RenderFunc,
//     Fake.ListFunc, Fake.LayersFunc). When set, it alone decides the result.
//   - When a Func is unset there is no natural default, so the method returns
//     an error wrapping ErrUnscripted that names the field to set. A fake
//     never panics. A call site that wants a fixed spec or a fixed rendering
//     sets the Func once, in a helper of its own.
//   - A Fake records only what some spec asserts on, through a method that
//     returns a deep copy (Fake.Renders: the name and vars of every Render
//     call, in call order, including calls whose RenderFunc failed or was
//     unset).
//   - The zero value is ready to use. Set the exported fields before the
//     first call: method calls are safe for concurrent use, writes to the
//     exported fields are not.
//   - A Func cannot call the default behavior (there is none to call). To
//     observe calls on top of different behavior, embed *Fake and override
//     the method.
//
// Production code must not import this package.
package prompttest
