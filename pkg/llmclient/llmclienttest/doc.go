// Package llmclienttest provides test doubles for code that uses
// pkg/llmclient.
//
// # Fakes
//
// Fake fakes llmclient.Client and is scripted per spec.
//
// Fake validates requests the way the real client does (nil request, missing
// or invalid tenant ID, empty messages or input) and rejects them with
// llmclient.ErrInvalidRequest before the scripted Func runs, so a spec cannot
// pass on a request production would refuse. It does not fill in the default
// model, so a spec that needs a model must set it on the request.
//
// Fake follows the convention every shared fake in a pkg/<x>/<x>test package
// uses:
//
//   - A method that returns data or whose result depends on its arguments is
//     scripted by a <Method>Func field with the method's exact signature
//     (Fake.CompleteFunc, Fake.EmbedFunc). When set, it alone decides the
//     result; the call is still recorded if the fake records that method.
//   - An argument-free method that only returns an error is scripted by a
//     <Method>Err field (Fake.HealthErr).
//   - An unset <Method>Func makes the method return an error wrapping
//     ErrUnscripted that names the field to set, so an unexpected call fails
//     the spec instead of passing silently. A fake never panics. Methods with
//     a natural default (Health and Close return nil) use it instead.
//   - Fakes record only what some spec asserts on, through methods that
//     return copies (Fake.CompleteRequests).
//   - The zero value is ready to use. Set the exported fields before the
//     first call: method calls are safe for concurrent use, writes to the
//     exported fields are not.
//   - A Func cannot call the default behavior: calling the same method from
//     inside it recurses. To observe calls and keep the default behavior,
//     embed *Fake and override the method.
//
// Production code must not import this package.
package llmclienttest
