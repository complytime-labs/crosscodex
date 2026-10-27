// Package dbtest provides test helpers for code that uses pkg/db.
//
// # Fakes
//
// Conn fakes db.Connection and db.TenantConnection, Tx fakes db.Transaction,
// and Row and Rows build db.Row and db.Rows over fixed values.
//
// They follow the convention every shared fake in a pkg/<x>/<x>test package
// uses:
//
//   - A method that returns data or whose result depends on its arguments is
//     scripted by a <Method>Func field with the method's exact signature
//     (Tx.ExecFunc, Conn.BeginFunc). When set, it alone decides the result;
//     the call is still recorded if the fake records that method.
//   - An argument-free method that only returns an error is scripted by a
//     <Method>Err field (Tx.CommitErr, Tx.RollbackErr).
//   - Value objects are built by constructors (Row, Rows), not scripted.
//   - An unset <Method>Func makes the method return an error wrapping
//     ErrUnscripted that names the field to set, so an unexpected call fails
//     the spec instead of passing silently. A fake never panics. Methods with
//     a natural default (Commit, Rollback, Close return nil) use it instead.
//   - Fakes record only what some spec asserts on, through methods that
//     return copies (Tx.Execs, Tx.Commits, Tx.Rollbacks, Conn.Begins,
//     FakeRows.Closed).
//   - The zero value is ready to use. Set the exported fields before the
//     first call: method calls are safe for concurrent use, writes to the
//     exported fields are not.
//   - A Func cannot call the default behavior: calling the same method from
//     inside it recurses. To observe calls and keep the default behavior,
//     embed *Tx or *Conn and override the method.
//
// # Role credentials
//
// Helpers for integration tests and test provisioning tools, so no fixed
// role password lives in the repository. Generate one per run with
// NewRolePassword, or read the per-run value the integration Taskfile
// generates with RolePassword, and set it with the statement
// AlterRolePasswordSQL builds; SetRolePassword runs it as the superuser.
// RoleDSN rewrites the superuser DSN to log in as that role. TestID names
// per-run fixtures.
//
// Production code must not import this package.
package dbtest
