// Package analyzertest provides a test double for code that consumes
// pkg/analyzer plugins.
//
// # Stubs
//
// Stub[T] stubs analyzer.Analyzer[T]. Register it with analyzer.Register[T]
// like any analyzer; the registry and the analysis engine then drive it
// through the same path they drive a production analyzer. A Stub is the
// analyzer, so it does no input validation of its own: analyzer.Register
// rejects an invalid name (analyzer.ValidateName), duplicates and cycles,
// and the engine decides what input reaches GenerateWork. Name, DependsOn
// and ResultSchema return the configured fields as-is.
//
// Gaps: a Stub does not enforce the "safe for concurrent use" clause of the
// Analyzer contract on the functions a spec supplies; only the Stub's own
// recorder is guarded. A Stub emits no telemetry.
//
// Stub follows the convention every shared fake in a pkg/<x>/<x>test package
// uses:
//
//   - A method that returns data or whose result depends on its arguments is
//     scripted by a <Method>Func field with the method's exact signature
//     (Stub.GenerateWorkFunc, Stub.AggregateFunc). When set, it alone decides
//     the result. GenerateWork records its input before calling the Func, so
//     a Func that fails still shows up in Stub.Inputs.
//   - When a Func is unset there is no natural default, so the method returns
//     an error wrapping ErrUnscripted that names the field to set. A stub
//     never panics. A call site that wants "no tasks" or "a plain output"
//     sets those Funcs once, in a constructor of its own.
//   - Name, DependsOn and ResultSchema are plain data, set by the
//     AnalyzerName, Dependencies and Schema fields. DependsOn returns a copy,
//     so a caller that sorts or edits the slice cannot change the stub.
//     ResultSchema returns Schema, which is nil when unset; set it when the
//     code under test reads the schema.
//   - A Stub records only what some spec asserts on, through a method that
//     returns a deep copy (Stub.Inputs: the proto inputs GenerateWork saw, in
//     call order).
//   - The zero value is ready to use. Set the exported fields before the
//     first call: method calls are safe for concurrent use, writes to the
//     exported fields are not.
//   - A Func cannot call the default behavior there is none to call. To
//     observe calls on top of different behavior, embed *Stub and override
//     the method.
//
// Production code must not import this package.
package analyzertest
