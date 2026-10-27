package dbtest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/complytime-labs/crosscodex/pkg/db"
)

// ErrUnscripted is wrapped by every error a fake returns for a call the test
// did not script. Match it with errors.Is; the message names the field to set.
var ErrUnscripted = errors.New("dbtest: call not scripted")

func unscripted(typ, method string) error {
	return fmt.Errorf("%w: %s called but %s.%sFunc is not set; set it to script this call",
		ErrUnscripted, method, typ, method)
}

// Statement is one recorded Exec call.
type Statement struct {
	Query string
	Args  []any
}

var (
	_ db.Transaction      = (*Tx)(nil)
	_ db.Connection       = (*Conn)(nil)
	_ db.TenantConnection = (*Conn)(nil)
	_ db.Rows             = (*FakeRows)(nil)
)

// Tx fakes db.Transaction. Unset Query/QueryRow/Exec return ErrUnscripted
// (QueryRow through the returned Row's Scan). Commit and Rollback default to
// nil. Rollback after a successful Commit is a no-op returning nil (the
// deferred-rollback idiom) until Conn.Begin hands this Tx out again. The zero
// value is ready to use. Set the exported fields before the first call; the
// methods are then safe for concurrent use.
type Tx struct {
	// QueryFunc, if set, alone decides what Query returns.
	QueryFunc func(ctx context.Context, query string, args ...any) (db.Rows, error)
	// QueryRowFunc, if set, alone decides what QueryRow returns.
	QueryRowFunc func(ctx context.Context, query string, args ...any) db.Row
	// ExecFunc, if set, alone decides what Exec returns. The call is
	// recorded in Execs either way.
	ExecFunc func(ctx context.Context, query string, args ...any) error
	// CommitErr is returned by every Commit. A failed Commit does not make
	// a later Rollback a no-op.
	CommitErr error
	// RollbackErr is returned by every Rollback that is not a no-op.
	RollbackErr error

	mu        sync.Mutex
	execs     []Statement
	commits   int
	rollbacks int
	committed bool
}

// begin starts a new transaction on a reused Tx: a Commit from an earlier
// transaction must not turn this one's Rollback into a no-op.
func (t *Tx) begin() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.committed = false
}

// Commit counts the call and returns CommitErr.
func (t *Tx) Commit() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.commits++
	if t.CommitErr == nil {
		t.committed = true
	}
	return t.CommitErr
}

// Rollback returns nil without counting after a successful Commit;
// otherwise it counts the call and returns RollbackErr.
func (t *Tx) Rollback() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.committed {
		return nil
	}
	t.rollbacks++
	return t.RollbackErr
}

// Query returns QueryFunc's result, or ErrUnscripted if it is unset.
func (t *Tx) Query(ctx context.Context, query string, args ...any) (db.Rows, error) {
	if t.QueryFunc == nil {
		return nil, unscripted("Tx", "Query")
	}
	return t.QueryFunc(ctx, query, args...)
}

// QueryRow returns QueryRowFunc's result, or a Row whose Scan returns
// ErrUnscripted if it is unset.
func (t *Tx) QueryRow(ctx context.Context, query string, args ...any) db.Row {
	if t.QueryRowFunc == nil {
		return Row(unscripted("Tx", "QueryRow"))
	}
	return t.QueryRowFunc(ctx, query, args...)
}

// Exec records the statement, then returns ExecFunc's result, or
// ErrUnscripted if it is unset.
func (t *Tx) Exec(ctx context.Context, query string, args ...any) error {
	t.mu.Lock()
	t.execs = append(t.execs, Statement{Query: query, Args: append([]any(nil), args...)})
	t.mu.Unlock()
	if t.ExecFunc == nil {
		return unscripted("Tx", "Exec")
	}
	return t.ExecFunc(ctx, query, args...)
}

// Execs returns a copy of every Exec call, in call order.
func (t *Tx) Execs() []Statement {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Statement, len(t.execs))
	for i, s := range t.execs {
		out[i] = Statement{Query: s.Query, Args: append([]any(nil), s.Args...)}
	}
	return out
}

// Commits returns how many times Commit was called, failed calls included.
func (t *Tx) Commits() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.commits
}

// Rollbacks returns how many Rollback calls were not no-ops.
func (t *Tx) Rollbacks() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rollbacks
}

// Conn fakes db.Connection (and so db.TenantConnection). Begin returns
// BeginFunc's result if set, else Tx if non-nil, else ErrUnscripted.
// Query/QueryRow/Exec behave as on Tx. Close returns nil. The zero value is
// ready to use. Set the exported fields before the first call; the methods
// are then safe for concurrent use.
type Conn struct {
	// Tx is returned by Begin when BeginFunc is unset. Each Begin starts a
	// new transaction on it, so a Commit in one transaction does not make
	// the next one's Rollback a no-op.
	Tx *Tx
	// BeginFunc, if set, alone decides what Begin returns.
	BeginFunc func(ctx context.Context) (db.Transaction, error)
	// QueryFunc, if set, alone decides what Query returns.
	QueryFunc func(ctx context.Context, query string, args ...any) (db.Rows, error)
	// QueryRowFunc, if set, alone decides what QueryRow returns.
	QueryRowFunc func(ctx context.Context, query string, args ...any) db.Row
	// ExecFunc, if set, alone decides what Exec returns.
	ExecFunc func(ctx context.Context, query string, args ...any) error

	mu     sync.Mutex
	begins int
}

// Begin counts the call, then returns BeginFunc's result, else Tx, else
// ErrUnscripted.
func (c *Conn) Begin(ctx context.Context) (db.Transaction, error) {
	c.mu.Lock()
	c.begins++
	c.mu.Unlock()
	if c.BeginFunc != nil {
		return c.BeginFunc(ctx)
	}
	if c.Tx != nil {
		c.Tx.begin()
		return c.Tx, nil
	}
	return nil, fmt.Errorf("%w: Begin called but Conn.BeginFunc and Conn.Tx are both unset; set one to script this call",
		ErrUnscripted)
}

// Query returns QueryFunc's result, or ErrUnscripted if it is unset.
func (c *Conn) Query(ctx context.Context, query string, args ...any) (db.Rows, error) {
	if c.QueryFunc == nil {
		return nil, unscripted("Conn", "Query")
	}
	return c.QueryFunc(ctx, query, args...)
}

// QueryRow returns QueryRowFunc's result, or a Row whose Scan returns
// ErrUnscripted if it is unset.
func (c *Conn) QueryRow(ctx context.Context, query string, args ...any) db.Row {
	if c.QueryRowFunc == nil {
		return Row(unscripted("Conn", "QueryRow"))
	}
	return c.QueryRowFunc(ctx, query, args...)
}

// Exec returns ExecFunc's result, or ErrUnscripted if it is unset.
func (c *Conn) Exec(ctx context.Context, query string, args ...any) error {
	if c.ExecFunc == nil {
		return unscripted("Conn", "Exec")
	}
	return c.ExecFunc(ctx, query, args...)
}

// Close returns nil.
func (c *Conn) Close() error { return nil }

// Begins returns how many times Begin was called, failed calls included.
func (c *Conn) Begins() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.begins
}

type fakeRow struct {
	err  error
	vals []any
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return assign(dest, r.vals)
}

// Row returns a db.Row whose Scan assigns vals to dest positionally, or
// returns err if non-nil.
//
// dest[i] must be a non-nil pointer, and vals[i] must be assignable to its
// element type, or convertible without loss between numeric kinds, or
// convertible to a named type of the same string or bool kind. Assigned
// []byte values are copied, as database/sql copies into *[]byte. A nil
// vals[i] is SQL NULL: it sets the zero value of a pointer, interface, slice
// or map destination and is an error for any other destination, as in
// database/sql.
//
// Scan returns an error naming the index and both types for anything else,
// including: a length mismatch; sql.Scanner destinations such as
// *sql.NullString (Scan is never called); a T value into a **T destination;
// []byte into *string and string into *[]byte; and a lossy conversion such
// as 300 into *int8, -1 into *uint, 1.5 into *int or 0.1 into *float32.
func Row(err error, vals ...any) db.Row {
	return fakeRow{err: err, vals: vals}
}

// FakeRows fakes db.Rows over fixed rows. Build it with Rows.
type FakeRows struct {
	// ScanErr, if non-nil, is returned by every Scan, so a test can fail a
	// scan that Row's rule would accept. Set it before the first call.
	ScanErr error

	mu      sync.Mutex
	rows    [][]any
	iterErr error
	pos     int
	ended   bool
	closed  bool
}

// Rows returns a FakeRows that iterates rows, each scanned with Row's rule.
// Err returns iterErr once iteration has ended (Next returned false or Close
// was called); Close is idempotent and recorded. Its methods are safe for
// concurrent use.
func Rows(iterErr error, rows ...[]any) *FakeRows {
	return &FakeRows{rows: rows, iterErr: iterErr}
}

// Next advances to the next row, returning false once rows are exhausted or
// after Close.
func (r *FakeRows) Next() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.pos >= len(r.rows) {
		r.ended = true
		r.pos = len(r.rows) + 1
		return false
	}
	r.pos++
	return true
}

// Scan returns ScanErr if set; otherwise it assigns the current row to dest
// with Row's rule, or errors when there is no current row.
func (r *FakeRows) Scan(dest ...any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ScanErr != nil {
		return r.ScanErr
	}
	if r.pos == 0 || r.pos > len(r.rows) {
		return errors.New("dbtest: Scan called without a current row; call Next first and scan only while it returns true")
	}
	return assign(dest, r.rows[r.pos-1])
}

// Close ends iteration and records the call. It always returns nil.
func (r *FakeRows) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.ended = true
	return nil
}

// Err returns iterErr once iteration has ended, nil before.
func (r *FakeRows) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.ended {
		return nil
	}
	return r.iterErr
}

// Closed reports whether Close was called.
func (r *FakeRows) Closed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

func assign(dest, vals []any) error {
	if len(dest) != len(vals) {
		return fmt.Errorf("dbtest: Scan got %d destinations for %d values; pass one destination per column",
			len(dest), len(vals))
	}
	for i, d := range dest {
		dv := reflect.ValueOf(d)
		if dv.Kind() != reflect.Pointer {
			return fmt.Errorf("dbtest: Scan destination %d is %T, not a pointer; pass the address of a variable", i, d)
		}
		if dv.IsNil() {
			return fmt.Errorf("dbtest: Scan destination %d is a nil %T; pass the address of a variable", i, d)
		}
		elem := dv.Elem()
		if vals[i] == nil {
			switch elem.Kind() {
			case reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map:
				elem.SetZero()
				continue
			}
			return fmt.Errorf("dbtest: Scan value %d: converting NULL to %s is unsupported; scan into a pointer or nullable type",
				i, elem.Type())
		}
		v := reflect.ValueOf(vals[i])
		if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 && !v.IsNil() {
			clone := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
			reflect.Copy(clone, v)
			v = clone
		}
		if v.Type().AssignableTo(elem.Type()) {
			elem.Set(v)
			continue
		}
		converted, ok := convert(v, elem.Type())
		if !ok {
			return fmt.Errorf("dbtest: Scan cannot store value %d (%s %v) in destination of type %s",
				i, v.Type(), v, elem.Type())
		}
		elem.Set(converted)
	}
	return nil
}

// convert converts v to t when both are numeric and the value survives the
// round trip, or when both share a string or bool kind (a named type).
func convert(v reflect.Value, t reflect.Type) (reflect.Value, bool) {
	from, to := v.Kind(), t.Kind()
	if from == to && (from == reflect.String || from == reflect.Bool) {
		return v.Convert(t), true
	}
	if !isNumeric(from) || !isNumeric(to) {
		return reflect.Value{}, false
	}
	c := v.Convert(t)
	if !c.Convert(v.Type()).Equal(v) || isNegative(c) != isNegative(v) {
		return reflect.Value{}, false
	}
	return c, true
}

func isNegative(v reflect.Value) bool {
	switch {
	case v.CanInt():
		return v.Int() < 0
	case v.CanFloat():
		return v.Float() < 0
	}
	return false
}

func isNumeric(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}
