//go:build !integration

package analyzer_test

import (
	"errors"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/analyzer"
	"github.com/complytime-labs/crosscodex/internal/testspecs"
	pkganalyzer "github.com/complytime-labs/crosscodex/pkg/analyzer"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestAnalyzerBDD(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Internal Analyzer BDD Suite")
}

var _ = BeforeSuite(func() { DeferCleanup(testspecs.RedirectLogsToGinkgo()) })

var _ = Describe("CountResults", func() {
	DescribeTable("counting result categories",
		func(results []pkganalyzer.TaskResult, wantProcessed, wantSkipped, wantErrors int) {
			processed, skipped, errors := analyzer.CountResults(results)
			Expect(processed).To(Equal(wantProcessed))
			Expect(skipped).To(Equal(wantSkipped))
			Expect(errors).To(Equal(wantErrors))
		},
		Entry("empty slice", nil, 0, 0, 0),
		Entry("all processed",
			[]pkganalyzer.TaskResult{
				{Result: &pb.AnalysisResult{Attributes: map[string]string{"type": "Technical|Tactical"}}},
				{Result: &pb.AnalysisResult{Attributes: map[string]string{"type": "Procedural|Strategic"}}},
			}, 2, 0, 0),
		Entry("all errors",
			[]pkganalyzer.TaskResult{
				{Error: errTest},
				{Error: errTest},
			}, 0, 0, 2),
		Entry("all skipped",
			[]pkganalyzer.TaskResult{
				{Result: &pb.AnalysisResult{Attributes: map[string]string{"skipped": "true"}}},
				{Result: &pb.AnalysisResult{Attributes: map[string]string{"skipped": "true"}}},
			}, 0, 2, 0),
		Entry("mixed: processed, skipped, errors",
			[]pkganalyzer.TaskResult{
				{Result: &pb.AnalysisResult{Attributes: map[string]string{"type": "Technical|Tactical"}}},
				{Result: &pb.AnalysisResult{Attributes: map[string]string{"skipped": "true"}}},
				{Error: errTest},
			}, 1, 1, 1),
		Entry("type assertion failure counts as error",
			[]pkganalyzer.TaskResult{
				{Result: &structpb.Value{}},
			}, 0, 0, 1),
		Entry("nil result with no error counts as error",
			[]pkganalyzer.TaskResult{
				{},
			}, 0, 0, 1),
	)
})

var _ = Describe("ExtractResponseText", func() {
	DescribeTable("extracting raw response text",
		func(result *structpb.Struct, wantRaw string, wantOK bool) {
			var msg proto.Message
			if result != nil {
				msg = result
			}
			raw, ok := analyzer.ExtractResponseText(msg)
			Expect(ok).To(Equal(wantOK))
			Expect(raw).To(Equal(wantRaw))
		},
		Entry("struct with response field", mustStruct(map[string]interface{}{"response": "hello"}), "hello", true),
		Entry("struct missing response field", mustStruct(map[string]interface{}{"other": "value"}), "", false),
		Entry("nil result", nil, "", false),
	)

	It("returns ok=false for a non-struct proto.Message", func() {
		raw, ok := analyzer.ExtractResponseText(&pb.AnalysisResult{})
		Expect(ok).To(BeFalse())
		Expect(raw).To(Equal(""))
	})
})

func mustStruct(fields map[string]interface{}) *structpb.Struct {
	s, err := structpb.NewStruct(fields)
	if err != nil {
		panic(err)
	}
	return s
}

var errTest = errors.New("test error")
