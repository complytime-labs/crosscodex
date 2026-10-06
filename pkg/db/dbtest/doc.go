// Package dbtest provides PostgreSQL role-credential helpers for integration
// tests and test provisioning tools. Use them so no fixed role password
// lives in the repository: generate one per run with NewRolePassword, or
// read the per-run value the integration Taskfile generates with
// RolePassword, and set it with the statement AlterRolePasswordSQL builds.
//
// Production code must not import this package.
package dbtest
