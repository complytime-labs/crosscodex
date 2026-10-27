package dbtest_test

import (
	"context"
	"math"
	"math/big"
	"reflect"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
)

var _ = Describe("Property Specifications", Ordered, func() {
	It("Tx.Execs returns every executed statement in call order", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			want := rapid.SliceOf(rapid.Custom(func(t *rapid.T) dbtest.Statement {
				return dbtest.Statement{
					Query: rapid.String().Draw(t, "query"),
					// Exec records no arguments as nil, so draw none as nil.
					Args: rapid.Map(rapid.SliceOfN(rapid.Int().AsAny(), 0, 4), func(a []any) []any {
						if len(a) == 0 {
							return nil
						}
						return a
					}).Draw(t, "args"),
				}
			})).Draw(t, "statements")

			tx := &dbtest.Tx{ExecFunc: func(context.Context, string, ...any) error { return nil }}
			for _, s := range want {
				if err := tx.Exec(context.Background(), s.Query, s.Args...); err != nil {
					t.Fatalf("scripted Exec failed: %v", err)
				}
			}
			got := tx.Execs()
			if len(want) == 0 && len(got) == 0 {
				return
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Execs() = %v, want %v", got, want)
			}
		})
	})

	It("Row converts an int64 to a numeric destination exactly when the value is representable", func() {
		targets := []reflect.Type{
			reflect.TypeFor[int8](), reflect.TypeFor[int16](), reflect.TypeFor[int32](),
			reflect.TypeFor[uint](), reflect.TypeFor[uint8](),
			reflect.TypeFor[float32](), reflect.TypeFor[float64](),
		}
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			n := rapid.OneOf(rapid.Int64(), rapid.Int64Range(-70000, 70000)).Draw(t, "n")
			target := rapid.SampledFrom(targets).Draw(t, "target")

			dest := reflect.New(target)
			err := dbtest.Row(nil, n).Scan(dest.Interface())
			if want := representable(t, n, target.Kind()); want != (err == nil) {
				t.Fatalf("Scan(%d) into *%s: want accepted=%v, got err=%v", n, target, want, err)
			}
			if err != nil {
				return
			}
			if back := dest.Elem().Convert(reflect.TypeFor[int64]()).Int(); back != n {
				t.Fatalf("Scan(%d) into *%s stored %v, which reads back as %d", n, target, dest.Elem(), back)
			}
		})
	})

	It("Row round-trips values of basic kinds into pointers of the same type", func() {
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			vals := rapid.SliceOf(rapid.OneOf(
				rapid.String().AsAny(),
				rapid.Int64().AsAny(),
				rapid.Bool().AsAny(),
				rapid.Float64Range(-1e12, 1e12).AsAny(),
				rapid.SliceOf(rapid.Byte()).AsAny(),
			)).Draw(t, "vals")

			dest := make([]any, len(vals))
			for i, v := range vals {
				dest[i] = reflect.New(reflect.TypeOf(v)).Interface()
			}
			if err := dbtest.Row(nil, vals...).Scan(dest...); err != nil {
				t.Fatalf("Scan(%v): %v", vals, err)
			}
			for i, d := range dest {
				if got := reflect.ValueOf(d).Elem().Interface(); !reflect.DeepEqual(got, vals[i]) {
					t.Fatalf("value %d: got %#v, want %#v", i, got, vals[i])
				}
			}
		})
	})
})

// representable is the oracle for the conversion property: whether n fits
// the kind exactly, computed without reflect conversions.
func representable(t *rapid.T, n int64, k reflect.Kind) bool {
	switch k {
	case reflect.Int8:
		return n >= math.MinInt8 && n <= math.MaxInt8
	case reflect.Int16:
		return n >= math.MinInt16 && n <= math.MaxInt16
	case reflect.Int32:
		return n >= math.MinInt32 && n <= math.MaxInt32
	case reflect.Uint:
		return n >= 0
	case reflect.Uint8:
		return n >= 0 && n <= math.MaxUint8
	case reflect.Float32:
		_, acc := new(big.Float).SetInt64(n).Float32()
		return acc == big.Exact
	case reflect.Float64:
		_, acc := new(big.Float).SetInt64(n).Float64()
		return acc == big.Exact
	}
	t.Fatalf("representable: no oracle for kind %s", k)
	return false
}
