package pipeline_test

import "context"

// acceptExec is a dbtest ExecFunc that accepts every statement, for specs
// that need Exec (such as enter_tenant) to succeed but assert nothing on it.
func acceptExec(context.Context, string, ...any) error { return nil }
