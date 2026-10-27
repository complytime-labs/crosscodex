// Package natsbustest provides test doubles for code that uses pkg/natsbus.
//
// # Fakes
//
// Fake fakes natsbus.Client and is scripted per spec. Sub fakes the
// natsbus.Subscription Fake hands out.
//
// Fake mirrors the checks the real client applies at its boundary, with the
// same sentinel errors and wording, and applies them before any scripted Func
// so a spec cannot pass on a call production would refuse:
//
//   - After Close, every method except Close returns natsbus.ErrConnectionClosed.
//   - Publish and PublishWithHeaders require a tenant in the context
//     (tenant.FromContext); without one they return the tenant error.
//
// A rejected call is not recorded, so a spec asserting that code under test
// must not publish needs a tenant-scoped context (tenant.WithTenant);
// otherwise a regressed publish is rejected unrecorded and the assertion
// passes vacuously. Gaps: the real client leaves subject and
// queue-name syntax to the nats.go library (empty subjects, spaces), which has
// no exported validator, so Fake accepts any subject and queue. The real
// client also accepts a nil handler (it would only fail on delivery); Fake
// accepts it too and Deliver skips it. Fake adds no provenance headers and
// does not verify them.
//
// # Delivering messages
//
// Fake.Deliver hands a message to the handler of every live subscription
// whose subject matches the message subject, synchronously, in subscription
// order. Handlers receive a context built from context.Background(), as in
// the real client: the tenant arrives in msg.Metadata.TenantID, never in the
// context, so a handler that needs a tenant context must build it itself. A
// subscription is live until Unsubscribe or Drain is called on it.
// Matching is delegated to nats-server's server.SubjectMatchesFilter, so it
// follows NATS rules exactly: "*" matches one token and a trailing ">" matches
// one or more tokens.
// Sub.Handle calls one subscription's handler directly, bypassing matching
// and liveness, for specs that feed a handler a subject the broker would
// never route to it.
//
// Fake follows the convention every shared fake in a pkg/<x>/<x>test package
// uses:
//
//   - A method that returns data or whose result depends on its arguments is
//     scripted by a <Method>Func field with the method's exact signature
//     (Fake.CreateStreamFunc, Fake.PublishFunc). When set, it alone decides
//     the result; an accepted call is still recorded if the fake records that
//     method. Fake has no argument-free error-only method to script, so it
//     has no <Method>Err field; Sub, which fakes such methods, uses
//     Sub.UnsubscribeErr and Sub.DrainErr.
//   - Publish and PublishWithHeaders record and return nil, Subscribe and
//     QueueSubscribe record and return a Sub, and Close returns nil. These
//     are the natural defaults. An unset CreateStreamFunc, DeleteStreamFunc or
//     AuditStreamRetentionFunc makes the method return an error wrapping
//     ErrUnscripted that names the field to set, so an unexpected call fails
//     the spec instead of passing silently. A fake never panics.
//   - Fakes record only what some spec asserts on, through methods that
//     return copies (Fake.Published, Fake.Subscriptions).
//   - The zero value is ready to use. Set the exported fields before the
//     first call: method calls are safe for concurrent use, writes to the
//     exported fields are not.
//   - A Func cannot call the default behavior: calling the same method from
//     inside it recurses. To observe calls and keep the default behavior,
//     embed *Fake and override the method.
//
// Production code must not import this package.
package natsbustest
