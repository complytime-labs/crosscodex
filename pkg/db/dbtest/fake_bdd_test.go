package dbtest_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
)

var errScripted = errors.New("scripted")

// octet is a named byte type: a []octet is a byte slice that []byte cannot
// be converted to.
type octet uint8

var _ = Describe("Tx", func() {
	var (
		ctx context.Context
		tx  *dbtest.Tx
	)

	BeforeEach(func() {
		ctx = context.Background()
		tx = &dbtest.Tx{}
	})

	DescribeTable("refuses an unscripted call, naming the field to set",
		func(call func(*dbtest.Tx) error, field string) {
			err := call(tx)
			Expect(err).To(MatchError(dbtest.ErrUnscripted))
			Expect(err.Error()).To(ContainSubstring(field))
		},
		Entry("Exec", func(t *dbtest.Tx) error { return t.Exec(context.Background(), "q") }, "Tx.ExecFunc"),
		Entry("Query", func(t *dbtest.Tx) error {
			rows, err := t.Query(context.Background(), "q")
			Expect(rows).To(BeNil())
			return err
		}, "Tx.QueryFunc"),
		Entry("QueryRow", func(t *dbtest.Tx) error {
			var s string
			return t.QueryRow(context.Background(), "q").Scan(&s)
		}, "Tx.QueryRowFunc"),
	)

	It("lets a scripted Func decide the result and still records the Exec", func() {
		tx.ExecFunc = func(_ context.Context, query string, args ...any) error {
			return fmt.Errorf("%w: %s %v", errScripted, query, args)
		}
		tx.QueryFunc = func(context.Context, string, ...any) (db.Rows, error) { return nil, errScripted }
		tx.QueryRowFunc = func(context.Context, string, ...any) db.Row { return dbtest.Row(errScripted) }

		Expect(tx.Exec(ctx, "DELETE", 1)).To(MatchError(errScripted))
		_, err := tx.Query(ctx, "SELECT")
		Expect(err).To(MatchError(errScripted))
		Expect(tx.QueryRow(ctx, "SELECT").Scan()).To(MatchError(errScripted))
		Expect(tx.Execs()).To(Equal([]dbtest.Statement{{Query: "DELETE", Args: []any{1}}}))
	})

	It("records every Exec in call order", func() {
		tx.ExecFunc = func(context.Context, string, ...any) error { return nil }
		Expect(tx.Exec(ctx, "a", "x")).To(Succeed())
		Expect(tx.Exec(ctx, "b")).To(Succeed())
		Expect(tx.Exec(ctx, "c", 1, 2)).To(Succeed())
		Expect(tx.Execs()).To(Equal([]dbtest.Statement{
			{Query: "a", Args: []any{"x"}},
			{Query: "b", Args: nil},
			{Query: "c", Args: []any{1, 2}},
		}))
	})

	It("returns a copy from Execs so callers cannot rewrite the record", func() {
		tx.ExecFunc = func(context.Context, string, ...any) error { return nil }
		Expect(tx.Exec(ctx, "a")).To(Succeed())
		got := tx.Execs()
		got[0].Query = "changed"
		Expect(tx.Execs()[0].Query).To(Equal("a"))
	})

	It("commits and rolls back with nil by default and counts each", func() {
		Expect(tx.Rollback()).To(Succeed())
		Expect(tx.Commit()).To(Succeed())
		Expect(tx.Commits()).To(Equal(1))
		Expect(tx.Rollbacks()).To(Equal(1))
	})

	It("treats Rollback after a successful Commit as an uncounted no-op", func() {
		tx.RollbackErr = errScripted
		Expect(tx.Commit()).To(Succeed())
		Expect(tx.Rollback()).To(Succeed())
		Expect(tx.Rollbacks()).To(Equal(0))
	})

	It("returns CommitErr and still counts a Rollback after the failed Commit", func() {
		tx.CommitErr = errScripted
		tx.RollbackErr = errors.New("rollback failed")
		Expect(tx.Commit()).To(MatchError(errScripted))
		Expect(tx.Rollback()).To(MatchError("rollback failed"))
		Expect(tx.Commits()).To(Equal(1))
		Expect(tx.Rollbacks()).To(Equal(1))
	})

	It("records 20 concurrent Execs", func() {
		tx.ExecFunc = func(context.Context, string, ...any) error { return nil }
		var wg sync.WaitGroup
		for i := range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer GinkgoRecover()
				Expect(tx.Exec(ctx, "INSERT", i)).To(Succeed())
			}()
		}
		wg.Wait()
		Expect(tx.Execs()).To(HaveLen(20))
	})
})

var _ = Describe("Conn", func() {
	var ctx context.Context

	BeforeEach(func() { ctx = context.Background() })

	DescribeTable("refuses an unscripted call, naming the field to set",
		func(call func(*dbtest.Conn) error, field string) {
			err := call(&dbtest.Conn{})
			Expect(err).To(MatchError(dbtest.ErrUnscripted))
			Expect(err.Error()).To(ContainSubstring(field))
		},
		Entry("Begin", func(c *dbtest.Conn) error {
			tx, err := c.Begin(context.Background())
			Expect(tx).To(BeNil())
			return err
		}, "Conn.BeginFunc"),
		Entry("Exec", func(c *dbtest.Conn) error { return c.Exec(context.Background(), "q") }, "Conn.ExecFunc"),
		Entry("Query", func(c *dbtest.Conn) error {
			_, err := c.Query(context.Background(), "q")
			return err
		}, "Conn.QueryFunc"),
		Entry("QueryRow", func(c *dbtest.Conn) error {
			return c.QueryRow(context.Background(), "q").Scan()
		}, "Conn.QueryRowFunc"),
	)

	It("lets scripted Funcs decide the result", func() {
		conn := &dbtest.Conn{
			ExecFunc:     func(context.Context, string, ...any) error { return errScripted },
			QueryFunc:    func(context.Context, string, ...any) (db.Rows, error) { return nil, errScripted },
			QueryRowFunc: func(context.Context, string, ...any) db.Row { return dbtest.Row(errScripted) },
		}
		Expect(conn.Exec(ctx, "q")).To(MatchError(errScripted))
		_, err := conn.Query(ctx, "q")
		Expect(err).To(MatchError(errScripted))
		Expect(conn.QueryRow(ctx, "q").Scan()).To(MatchError(errScripted))
		Expect(conn.Close()).To(Succeed())
	})

	It("begins with BeginFunc over Tx", func() {
		other := &dbtest.Tx{}
		conn := &dbtest.Conn{
			Tx:        &dbtest.Tx{},
			BeginFunc: func(context.Context) (db.Transaction, error) { return other, nil },
		}
		tx, err := conn.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(tx).To(BeIdenticalTo(other))
	})

	It("begins with Tx when BeginFunc is unset", func() {
		conn := &dbtest.Conn{Tx: &dbtest.Tx{}}
		tx, err := conn.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(tx).To(BeIdenticalTo(conn.Tx))
	})

	It("starts a new transaction on a reused Tx, so an earlier Commit does not swallow the next Rollback", func() {
		conn := &dbtest.Conn{Tx: &dbtest.Tx{}}
		tx, err := conn.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(tx.Commit()).To(Succeed())
		tx, err = conn.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(tx.Rollback()).To(Succeed())
		Expect(conn.Tx.Rollbacks()).To(Equal(1))
	})

	It("counts every Begin, failed ones included", func() {
		conn := &dbtest.Conn{BeginFunc: func(context.Context) (db.Transaction, error) { return nil, errScripted }}
		_, _ = conn.Begin(ctx)
		_, _ = conn.Begin(ctx)
		Expect(conn.Begins()).To(Equal(2))
	})
})

var _ = Describe("Row", func() {
	It("returns its error from Scan without touching the destinations", func() {
		s := "kept"
		Expect(dbtest.Row(errScripted, "v").Scan(&s)).To(MatchError(errScripted))
		Expect(s).To(Equal("kept"))
	})

	It("assigns string, int64, time.Time, []byte, bool and nil positionally", func() {
		now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		var (
			s  string
			n  int64
			t  time.Time
			b  []byte
			ok bool
			z  = new(string)
		)
		Expect(dbtest.Row(nil, "s", int64(7), now, []byte("raw"), true, nil).Scan(&s, &n, &t, &b, &ok, &z)).To(Succeed())
		Expect(s).To(Equal("s"))
		Expect(n).To(Equal(int64(7)))
		Expect(t).To(Equal(now))
		Expect(b).To(Equal([]byte("raw")))
		Expect(ok).To(BeTrue())
		Expect(z).To(BeNil())
	})

	DescribeTable("scans NULL into a nullable destination as its zero value",
		func(dest any) {
			Expect(dbtest.Row(nil, nil).Scan(dest)).To(Succeed())
			Expect(reflect.ValueOf(dest).Elem().IsZero()).To(BeTrue())
		},
		Entry("pointer", func() any { p := new(string); return &p }()),
		Entry("interface", func() any { var v any = "x"; return &v }()),
		Entry("slice", &[]byte{1}),
		Entry("map", &map[string]int{"a": 1}),
	)

	It("copies a named-byte slice into a destination of the same type", func() {
		src := []octet{1, 2}
		var dst []octet
		Expect(dbtest.Row(nil, src).Scan(&dst)).To(Succeed())
		src[0] = 9
		Expect(dst).To(Equal([]octet{1, 2}))
	})

	It("copies []byte values so the caller's buffer and the row stay independent", func() {
		src := []byte("raw")
		var b []byte
		Expect(dbtest.Row(nil, src).Scan(&b)).To(Succeed())
		src[0] = 'X'
		Expect(b).To(Equal([]byte("raw")))
	})

	It("converts between numeric kinds when no value is lost", func() {
		var (
			f float64
			i int32
		)
		Expect(dbtest.Row(nil, 3, int64(9)).Scan(&f, &i)).To(Succeed())
		Expect(f).To(Equal(3.0))
		Expect(i).To(Equal(int32(9)))
	})

	It("assigns to a named type with the same underlying kind", func() {
		type status string
		var st status
		Expect(dbtest.Row(nil, "active").Scan(&st)).To(Succeed())
		Expect(st).To(Equal(status("active")))
	})

	DescribeTable("refuses a destination it cannot fill, naming the index and types",
		func(vals []any, dest []any, want []string) {
			err := dbtest.Row(nil, vals...).Scan(dest...)
			Expect(err).To(HaveOccurred())
			for _, w := range want {
				Expect(err.Error()).To(ContainSubstring(w))
			}
		},
		Entry("fewer destinations than values", []any{"a", "b"}, []any{new(string)}, []string{"1 destinations", "2 values"}),
		Entry("more destinations than values", []any{"a"}, []any{new(string), new(string)}, []string{"2 destinations", "1 values"}),
		Entry("nil pointer", []any{"a"}, []any{(*string)(nil)}, []string{"destination 0", "*string", "nil"}),
		Entry("untyped nil", []any{"a"}, []any{nil}, []string{"destination 0", "not a pointer"}),
		Entry("non-pointer", []any{"a"}, []any{"x"}, []string{"destination 0", "string", "not a pointer"}),
		Entry("unassignable type", []any{"a", 1}, []any{new(string), new(bool)}, []string{"value 1", "int", "bool"}),
		Entry("int into string", []any{65}, []any{new(string)}, []string{"value 0", "int", "string"}),
		Entry("lossy numeric conversion", []any{300}, []any{new(int8)}, []string{"value 0", "int", "int8"}),
		Entry("fractional float into int", []any{1.5}, []any{new(int)}, []string{"value 0", "float64", "int"}),
		Entry("lossy float64 into float32", []any{0.1}, []any{new(float32)}, []string{"value 0", "float64", "float32"}),
		Entry("NULL into a non-nullable destination", []any{nil}, []any{new(string)},
			[]string{"value 0", "converting NULL to string is unsupported"}),
		Entry("sql.Scanner destination", []any{"a"}, []any{new(sql.NullString)}, []string{"value 0", "string", "sql.NullString"}),
		Entry("T into **T", []any{"a"}, []any{new(*string)}, []string{"value 0", "string", "*string"}),
		Entry("[]byte into string", []any{[]byte("a")}, []any{new(string)}, []string{"value 0", "[]uint8", "string"}),
		Entry("string into []byte", []any{"a"}, []any{new([]byte)}, []string{"value 0", "string", "[]uint8"}),
		Entry("named-byte slice into []byte", []any{[]octet{1}}, []any{new([]byte)}, []string{"value 0", "dbtest_test.octet", "[]uint8"}),
		Entry("negative int into uint", []any{-1}, []any{new(uint)}, []string{"value 0", "int", "uint"}),
		Entry("huge uint64 into int64", []any{^uint64(0)}, []any{new(int64)}, []string{"value 0", "uint64", "int64"}),
	)
})

var _ = Describe("Rows", func() {
	It("iterates every row, scanning each with Row's rule, then reports iterErr", func() {
		rows := dbtest.Rows(errScripted, []any{"a", 1}, []any{"b", 2})
		var (
			names []string
			nums  []int
		)
		Expect(rows.Err()).To(Succeed(), "Err must stay nil until iteration ends")
		for rows.Next() {
			var (
				s string
				n int
			)
			Expect(rows.Scan(&s, &n)).To(Succeed())
			names = append(names, s)
			nums = append(nums, n)
		}
		Expect(names).To(Equal([]string{"a", "b"}))
		Expect(nums).To(Equal([]int{1, 2}))
		Expect(rows.Err()).To(MatchError(errScripted))
	})

	It("refuses Scan without a current row", func() {
		rows := dbtest.Rows(nil, []any{"a"})
		var s string
		Expect(rows.Scan(&s)).To(MatchError(ContainSubstring("call Next first")))
		Expect(rows.Next()).To(BeTrue())
		Expect(rows.Next()).To(BeFalse())
		Expect(rows.Scan(&s)).To(MatchError(ContainSubstring("call Next first")))
	})

	It("returns ScanErr from every Scan when set", func() {
		rows := dbtest.Rows(nil, []any{"a"})
		rows.ScanErr = errScripted
		Expect(rows.Next()).To(BeTrue())
		var s string
		Expect(rows.Scan(&s)).To(MatchError(errScripted))
	})

	It("records an idempotent Close, ends iteration and exposes iterErr", func() {
		rows := dbtest.Rows(errScripted, []any{"a"})
		Expect(rows.Closed()).To(BeFalse())
		Expect(rows.Close()).To(Succeed())
		Expect(rows.Close()).To(Succeed())
		Expect(rows.Closed()).To(BeTrue())
		Expect(rows.Next()).To(BeFalse())
		Expect(rows.Err()).To(MatchError(errScripted))
	})

	It("iterates nothing when given no rows", func() {
		rows := dbtest.Rows(nil)
		Expect(rows.Next()).To(BeFalse())
		Expect(rows.Err()).To(Succeed())
	})
})
