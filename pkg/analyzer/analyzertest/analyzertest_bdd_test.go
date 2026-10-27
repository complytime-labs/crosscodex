package analyzertest_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/analyzertest"
)

func TestAnalyzerTest(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "AnalyzerTest Suite")
}

var errScripted = errors.New("scripted")

var _ = Describe("Stub", func() {
	var (
		ctx  context.Context
		stub *analyzertest.Stub[*wrapperspb.StringValue]
	)

	BeforeEach(func() {
		ctx = context.Background()
		stub = &analyzertest.Stub[*wrapperspb.StringValue]{AnalyzerName: "classify"}
	})

	It("is usable as the zero value", func() {
		var zero analyzertest.Stub[*emptypb.Empty]
		Expect(zero.Name()).To(BeEmpty())
		Expect(zero.DependsOn()).To(BeEmpty())
		Expect(zero.ResultSchema()).To(BeNil())
	})

	It("returns the configured name, dependencies and schema", func() {
		stub.Dependencies = []string{"a", "b"}
		stub.Schema = &emptypb.Empty{}
		Expect(stub.Name()).To(Equal("classify"))
		Expect(stub.DependsOn()).To(Equal([]string{"a", "b"}))
		Expect(stub.ResultSchema()).To(Equal(proto.Message(&emptypb.Empty{})))
	})

	It("returns a DependsOn copy, so editing the result leaves the stub intact", func() {
		stub.Dependencies = []string{"b", "a"}
		got := stub.DependsOn()
		got[0] = "mutated"
		Expect(stub.DependsOn()).To(Equal([]string{"b", "a"}))
		Expect(stub.Dependencies).To(Equal([]string{"b", "a"}))
	})

	Describe("unscripted calls", func() {
		It("GenerateWork returns ErrUnscripted naming the field", func() {
			tasks, err := stub.GenerateWork(ctx, wrapperspb.String("x"), analyzer.AnalyzerConfig{})
			Expect(tasks).To(BeNil())
			Expect(err).To(MatchError(analyzertest.ErrUnscripted))
			Expect(err.Error()).To(ContainSubstring("Stub.GenerateWorkFunc is not set"))
		})

		It("Aggregate returns ErrUnscripted naming the field", func() {
			out, err := stub.Aggregate(ctx, nil)
			Expect(out).To(BeNil())
			Expect(err).To(MatchError(analyzertest.ErrUnscripted))
			Expect(err.Error()).To(ContainSubstring("Stub.AggregateFunc is not set"))
		})

		It("still records the input of an unscripted GenerateWork", func() {
			_, _ = stub.GenerateWork(ctx, wrapperspb.String("x"), analyzer.AnalyzerConfig{})
			Expect(stub.Inputs()).To(HaveLen(1))
		})
	})

	Describe("scripted calls", func() {
		It("GenerateWorkFunc alone decides the result, and receives the arguments", func() {
			var gotInput string
			var gotCfg analyzer.AnalyzerConfig
			stub.GenerateWorkFunc = func(_ context.Context, in *wrapperspb.StringValue, cfg analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
				gotInput, gotCfg = in.GetValue(), cfg
				return []analyzer.Task{{TaskID: "t1"}}, nil
			}
			cfg := analyzer.AnalyzerConfig{Parameters: map[string]string{"k": "v"}}
			tasks, err := stub.GenerateWork(ctx, wrapperspb.String("in"), cfg)
			Expect(err).NotTo(HaveOccurred())
			Expect(tasks).To(HaveLen(1))
			Expect(tasks[0].TaskID).To(Equal("t1"))
			Expect(gotInput).To(Equal("in"))
			Expect(gotCfg).To(Equal(cfg))
		})

		It("GenerateWorkFunc errors pass through unchanged and the input is still recorded", func() {
			stub.GenerateWorkFunc = func(context.Context, *wrapperspb.StringValue, analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
				return nil, errScripted
			}
			_, err := stub.GenerateWork(ctx, wrapperspb.String("in"), analyzer.AnalyzerConfig{})
			Expect(err).To(MatchError(errScripted))
			Expect(errors.Is(err, analyzertest.ErrUnscripted)).To(BeFalse())
			Expect(stub.Inputs()).To(HaveLen(1))
		})

		It("AggregateFunc alone decides the result, and receives the results and context", func() {
			type ctxKey struct{}
			var got []analyzer.TaskResult
			var gotCtx any
			stub.AggregateFunc = func(c context.Context, results []analyzer.TaskResult) (*analyzer.Output, error) {
				got, gotCtx = results, c.Value(ctxKey{})
				return &analyzer.Output{AnalyzerName: "classify"}, nil
			}
			in := []analyzer.TaskResult{{TaskID: "t1"}}
			out, err := stub.Aggregate(context.WithValue(ctx, ctxKey{}, "v"), in)
			Expect(err).NotTo(HaveOccurred())
			Expect(out.AnalyzerName).To(Equal("classify"))
			Expect(got).To(Equal(in))
			Expect(gotCtx).To(Equal("v"))
		})

		It("AggregateFunc errors pass through unchanged", func() {
			stub.AggregateFunc = func(context.Context, []analyzer.TaskResult) (*analyzer.Output, error) {
				return nil, errScripted
			}
			_, err := stub.Aggregate(ctx, nil)
			Expect(err).To(MatchError(errScripted))
			Expect(errors.Is(err, analyzertest.ErrUnscripted)).To(BeFalse())
		})
	})

	Describe("Inputs", func() {
		It("is empty before any call", func() {
			Expect(stub.Inputs()).To(BeEmpty())
		})

		It("records inputs in call order", func() {
			for _, v := range []string{"a", "b", "c"} {
				_, _ = stub.GenerateWork(ctx, wrapperspb.String(v), analyzer.AnalyzerConfig{})
			}
			var got []string
			for _, in := range stub.Inputs() {
				got = append(got, in.GetValue())
			}
			Expect(got).To(Equal([]string{"a", "b", "c"}))
		})

		It("deep-copies, so mutating the input or the result leaves the record intact", func() {
			in := wrapperspb.String("orig")
			_, _ = stub.GenerateWork(ctx, in, analyzer.AnalyzerConfig{})
			in.Value = "mutated-input"
			stub.Inputs()[0].Value = "mutated-result"
			Expect(stub.Inputs()[0].GetValue()).To(Equal("orig"))
		})

		It("records a nil input as nil without panicking", func() {
			_, err := stub.GenerateWork(ctx, nil, analyzer.AnalyzerConfig{})
			Expect(err).To(MatchError(analyzertest.ErrUnscripted))
			Expect(stub.Inputs()).To(HaveLen(1))
			Expect(stub.Inputs()[0]).To(BeNil())
		})

		It("records interface-typed inputs (T = proto.Message)", func() {
			iface := &analyzertest.Stub[proto.Message]{}
			_, _ = iface.GenerateWork(ctx, wrapperspb.String("x"), analyzer.AnalyzerConfig{})
			_, _ = iface.GenerateWork(ctx, nil, analyzer.AnalyzerConfig{})
			Expect(iface.Inputs()).To(HaveLen(2))
			Expect(proto.Equal(iface.Inputs()[0], wrapperspb.String("x"))).To(BeTrue())
			Expect(iface.Inputs()[1]).To(BeNil())
		})
	})

	Describe("through the registration path", func() {
		var reg *analyzer.Registry

		BeforeEach(func() { reg = analyzer.NewRegistry() })

		It("registers, resolves and drives GenerateWork and Aggregate via RegisteredAnalyzer", func() {
			stub.Dependencies = []string{"embedding"}
			stub.Schema = &emptypb.Empty{}
			stub.GenerateWorkFunc = func(_ context.Context, in *wrapperspb.StringValue, _ analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
				return []analyzer.Task{{TaskID: in.GetValue()}}, nil
			}
			stub.AggregateFunc = func(context.Context, []analyzer.TaskResult) (*analyzer.Output, error) {
				return &analyzer.Output{AnalyzerName: "classify"}, nil
			}
			dep := &analyzertest.Stub[*wrapperspb.StringValue]{AnalyzerName: "embedding"}
			Expect(analyzer.Register[*wrapperspb.StringValue](reg, dep)).To(Succeed())
			Expect(analyzer.Register[*wrapperspb.StringValue](reg, stub)).To(Succeed())

			ra, err := reg.Get("classify")
			Expect(err).NotTo(HaveOccurred())
			Expect(ra.DependsOn()).To(Equal([]string{"embedding"}))
			Expect(ra.InputType()).To(Equal(string((&wrapperspb.StringValue{}).ProtoReflect().Descriptor().FullName())))

			tasks, err := ra.GenerateWorkFromProto(ctx, wrapperspb.String("t9"), analyzer.AnalyzerConfig{})
			Expect(err).NotTo(HaveOccurred())
			Expect(tasks).To(HaveLen(1))
			Expect(tasks[0].TaskID).To(Equal("t9"))
			Expect(stub.Inputs()).To(HaveLen(1))

			out, err := ra.Aggregate(ctx, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(out.AnalyzerName).To(Equal("classify"))

			dag, err := reg.BuildDAG(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(dag).NotTo(BeNil())
		})

		It("surfaces ErrUnscripted through the wrapper", func() {
			Expect(analyzer.Register[*wrapperspb.StringValue](reg, stub)).To(Succeed())
			ra, err := reg.Get("classify")
			Expect(err).NotTo(HaveOccurred())
			_, err = ra.GenerateWorkFromProto(ctx, wrapperspb.String("x"), analyzer.AnalyzerConfig{})
			Expect(err).To(MatchError(analyzertest.ErrUnscripted))
			_, err = ra.Aggregate(ctx, nil)
			Expect(err).To(MatchError(analyzertest.ErrUnscripted))
		})

		It("does not validate its own name: the registry rejects an invalid one", func() {
			bad := &analyzertest.Stub[*emptypb.Empty]{AnalyzerName: "Not Valid"}
			err := analyzer.Register[*emptypb.Empty](reg, bad)
			Expect(err).To(MatchError(analyzer.ErrInvalidName))
		})

		It("a wrong-typed input never reaches the stub", func() {
			Expect(analyzer.Register[*wrapperspb.StringValue](reg, stub)).To(Succeed())
			ra, err := reg.Get("classify")
			Expect(err).NotTo(HaveOccurred())
			_, err = ra.GenerateWorkFromProto(ctx, &emptypb.Empty{}, analyzer.AnalyzerConfig{})
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, analyzertest.ErrUnscripted)).To(BeFalse())
			Expect(stub.Inputs()).To(BeEmpty())
		})
	})

	It("is safe for concurrent use", func() {
		stub.GenerateWorkFunc = func(context.Context, *wrapperspb.StringValue, analyzer.AnalyzerConfig) ([]analyzer.Task, error) {
			return nil, nil
		}
		stub.AggregateFunc = func(context.Context, []analyzer.TaskResult) (*analyzer.Output, error) {
			return &analyzer.Output{}, nil
		}
		var wg sync.WaitGroup
		for range 20 {
			wg.Add(1)
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				_, err := stub.GenerateWork(ctx, wrapperspb.String("x"), analyzer.AnalyzerConfig{})
				Expect(err).NotTo(HaveOccurred())
				_, err = stub.Aggregate(ctx, nil)
				Expect(err).NotTo(HaveOccurred())
				_ = stub.Inputs()
				_ = stub.DependsOn()
			}()
		}
		wg.Wait()
		Expect(stub.Inputs()).To(HaveLen(20))
	})
})
