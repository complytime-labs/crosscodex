// Package storagetest provides a test double for code that uses pkg/storage.
//
// # Fakes
//
// Fake fakes storage.Provider as a mutex-guarded in-memory object store. It
// behaves like the provider storage.NewLocal returns, so a spec exercises the
// same contract production code sees:
//
//   - Get and Stat return storage.ErrNotFound for a missing key. Delete of a
//     missing key and Exists of a missing key succeed (false for Exists).
//   - Every method that takes a key rejects it with storage.ErrInvalidKey
//     when storage.ValidateKey does (empty, absolute, "..", backslash, NUL),
//     as does List for a non-empty prefix. Keys that differ only in spelling
//     ("a//b", "a/./b", "a/b") name one object, as on disk.
//   - After Close, every method except Close returns
//     storage.ErrProviderClosed, and Close itself keeps returning nil (or
//     CloseErr).
//   - List treats its prefix as a path, not a string prefix: List("docs")
//     returns "docs/a" but not "docsx/b". It returns matches in Local's walk
//     order, compared segment by segment, so "a/x" precedes "a-b" (nil when
//     there are none).
//   - Put and Stat/List metadata fill Key, Size and LastModified (Unix
//     seconds at the time of the Put), the fields Local fills; ContentType,
//     ETag and Metadata stay empty.
//
// A rejected call is not stored and does not reach a scripted Func, so a spec
// asserting that code under test must not write needs keys the fake would
// accept; otherwise a regressed write is rejected and the assertion passes
// vacuously.
//
// Gaps: Local also fails a Put whose key would turn an existing object into a
// directory (or the reverse) and a Get of a directory; Fake treats keys as
// flat strings and allows both. Local rejects symlink escapes; an in-memory
// store has no symlinks. Local panics on a nil reader; Fake returns an error.
// Fake emits no telemetry.
//
// Fake follows the convention every shared fake in a pkg/<x>/<x>test package
// uses:
//
//   - A method that returns data or whose result depends on its arguments is
//     scripted by a <Method>Func field with the method's exact signature
//     (Fake.PutFunc, Fake.GetFunc). When set, it alone decides the result
//     after key validation and the closed check; a Put that PutFunc decides
//     stores nothing, and a Delete that DeleteFunc decides deletes nothing.
//   - An argument-free method that only returns an error is scripted by a
//     <Method>Err field (Fake.CloseErr). Close closes the provider even when
//     it returns CloseErr.
//   - Every method has a natural stateful default, so Fake has no
//     ErrUnscripted: an unset Func runs the in-memory behavior above. A fake
//     never panics.
//   - Fake records only what some spec asserts on, through a method that
//     returns a deep copy (Fake.Objects). Get, List and Stat results are
//     copies too.
//   - The zero value is ready to use. Set the exported fields before the
//     first call: method calls are safe for concurrent use, writes to the
//     exported fields are not.
//   - A Func cannot call the default behavior: calling the same method from
//     inside it recurses. To observe calls and keep the default behavior,
//     embed *Fake and override the method.
//
// Production code must not import this package.
package storagetest
