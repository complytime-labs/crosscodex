package config_test

import (
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/config"
)

var _ = Describe("FieldError", func() {
	field := func(path string) *config.FieldError {
		return &config.FieldError{Path: path, Err: fmt.Errorf("%s is wrong: %w", path, config.ErrInvalidConfig)}
	}

	It("reads as its error and unwraps to it", func() {
		fe := field("logging.level")
		Expect(fe.Error()).To(Equal("logging.level is wrong: invalid configuration"))
		Expect(fe).To(MatchError(config.ErrInvalidConfig))
	})

	DescribeTable("FieldErrors finds every field error in an error tree, in order",
		func(err error, want []string) {
			var got []string
			for _, fe := range config.FieldErrors(err) {
				got = append(got, fe.Path)
			}
			Expect(got).To(Equal(want))
		},
		Entry("nil", nil, nil),
		Entry("a bare field error", error(field("a")), []string{"a"}),
		Entry("a joined set", errors.Join(field("a"), field("b")), []string{"a", "b"}),
		Entry("wrapped once", fmt.Errorf("context: %w", field("a")), []string{"a"}),
		Entry("next to a sentinel", fmt.Errorf("%w: %w", config.ErrLoadFailed, errors.Join(field("a"), field("b"))), []string{"a", "b"}),
		Entry("a join wrapped once", fmt.Errorf("ctx: %w", errors.Join(field("a"), field("b"))), []string{"a", "b"}),
		Entry("nested joins", errors.Join(field("a"), errors.Join(field("b"), field("a"))), []string{"a", "b", "a"}),
		Entry("an untagged error only", fmt.Errorf("x: %w", config.ErrInvalidConfig), nil),
	)

	DescribeTable("FieldErrorsComplete reports whether field errors account for the whole tree",
		func(err error, want bool) {
			Expect(config.FieldErrorsComplete(err)).To(Equal(want))
		},
		Entry("a bare field error", error(field("a")), true),
		Entry("a joined set", errors.Join(field("a"), field("b")), true),
		Entry("wrapped once", fmt.Errorf("context: %w", field("a")), true),
		Entry("next to the package sentinels", fmt.Errorf("%w: %w", config.ErrLoadFailed, fmt.Errorf("%w: %w", config.ErrInvalidConfig, field("a"))), true),
		Entry("with an untagged sibling", errors.Join(field("a"), fmt.Errorf("x: %w", config.ErrInvalidConfig)), false),
		Entry("an untagged error", fmt.Errorf("x: %w", config.ErrInvalidConfig), false),
		Entry("a bare sentinel", config.ErrInvalidConfig, false),
		Entry("a sentinel joined next to a field error", errors.Join(field("a"), config.ErrInvalidConfig), false),
		Entry("a foreign error", errors.New("disk full"), false),
		Entry("nil", nil, false),
	)
})
