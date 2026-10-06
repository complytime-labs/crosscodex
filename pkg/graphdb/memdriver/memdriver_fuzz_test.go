package memdriver_test

import (
	"math"
	"testing"

	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
)

func sameValue(a, b any) bool {
	fa, aIsFloat := a.(float64)
	fb, bIsFloat := b.(float64)
	if aIsFloat && bIsFloat && math.IsNaN(fa) && math.IsNaN(fb) {
		return true
	}
	return a == b
}

// FuzzNormalizeValue checks that every property value normalizes to a type
// AGE can return (string, bool, float64) and that normalization is idempotent.
func FuzzNormalizeValue(f *testing.F) {
	f.Add("text", int64(42), 3.5, true, uint8(0))
	f.Add("", int64(0), 0.0, false, uint8(1))
	f.Add("x", int64(math.MaxInt64), math.MaxFloat64, true, uint8(2))
	f.Add("x", int64(math.MinInt64), -0.1, false, uint8(3))
	f.Add("x", int64(1), math.Inf(1), true, uint8(4))
	f.Add("x", int64(1), math.NaN(), true, uint8(4))
	f.Add("'; DROP TABLE x; --", int64(7), 1e-300, true, uint8(6))
	f.Add("$cypher$", int64(-1), 0.1, true, uint8(5))

	f.Fuzz(func(t *testing.T, s string, i int64, fl float64, b bool, kind uint8) {
		var in any
		switch kind % 7 {
		case 0:
			in = s
		case 1:
			in = int(i)
		case 2:
			in = i
		case 3:
			in = fl
		case 4:
			in = float32(fl)
		case 5:
			in = b
		default:
			in = []string{s}
		}

		out := memdriver.NormalizeValue(in)
		switch out.(type) {
		case string, bool, float64:
		default:
			t.Fatalf("normalizeValue(%T) returned %T; want string, bool or float64", in, out)
		}
		if again := memdriver.NormalizeValue(out); !sameValue(out, again) {
			t.Fatalf("normalizeValue not idempotent: %#v -> %#v -> %#v", in, out, again)
		}
	})
}
