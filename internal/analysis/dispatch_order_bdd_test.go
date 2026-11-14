package analysis_test

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/complytime-labs/crosscodex/internal/analysis"
	"github.com/complytime-labs/crosscodex/pkg/analyzer"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/analyzertest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// panelledStub is an analyzer that declares a panel (analyzer.Panelled).
type panelledStub struct {
	*analyzertest.Stub[*emptypb.Empty]
	aliases []string
}

func (s *panelledStub) Panel() analyzer.Panel { return analyzer.Panel{Aliases: s.aliases} }

// modelTask is a task whose payload names model, as every LLM analyzer's
// payload does. An empty model leaves the field out.
func modelTask(id, model string) analyzer.Task {
	fields := map[string]any{"pair": id}
	if model != "" {
		fields["model"] = model
	}
	payload, err := structpb.NewStruct(fields)
	Expect(err).NotTo(HaveOccurred())
	return analyzer.Task{TaskID: id, TaskType: "relationship", Payload: payload}
}

// pairTasks returns one task per pair per model, models innermost: the
// order a pairwise analyzer generates them in.
func pairTasks(pairs int, models ...string) []analyzer.Task {
	var tasks []analyzer.Task
	for p := range pairs {
		for _, m := range models {
			tasks = append(tasks, modelTask(fmt.Sprintf("p%d-%s", p, m), m))
		}
	}
	return tasks
}

func taskIDs(tasks []analyzer.Task) []string {
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		ids[i] = t.TaskID
	}
	return ids
}

var _ = Describe("Dispatch order", func() {
	var (
		registry   *analyzer.Registry
		dispatcher *engineTestDispatcher
		collector  *engineTestCollector
		ctx        context.Context
	)

	BeforeEach(func() {
		registry = analyzer.NewRegistry()
		dispatcher = &engineTestDispatcher{}
		collector = &engineTestCollector{}
		ctx, _ = tenant.WithTenant(context.Background(), "test-tenant")
	})

	run := func(a analyzer.Analyzer[*emptypb.Empty]) {
		Expect(analyzer.Register[*emptypb.Empty](registry, a)).To(Succeed())
		engine := analysis.New(registry, dispatcher, collector, validCfg(), allTaskTypes())
		result, err := engine.Execute(ctx, analysis.ExecutionRequest{JobID: "job-order", Input: &emptypb.Empty{}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Completed).To(ConsistOf("relationship"))
		Expect(dispatcher.dispatches).To(HaveLen(1))
	}

	DescribeTable("a stage's tasks are dispatched grouped by model",
		func(panel []string, generated []analyzer.Task, wantOrder []string) {
			stub := newStubWithTasks("relationship", generated)
			if panel == nil {
				run(stub)
			} else {
				run(&panelledStub{Stub: stub, aliases: panel})
			}
			Expect(taskIDs(dispatcher.dispatches[0].tasks)).To(Equal(wantOrder))
		},
		Entry("in the panel's configured order, generation order kept within a model",
			[]string{"panel-1", "panel-2", "panel-3"},
			pairTasks(2, "panel-1", "panel-2", "panel-3"),
			[]string{"p0-panel-1", "p1-panel-1", "p0-panel-2", "p1-panel-2", "p0-panel-3", "p1-panel-3"}),
		Entry("in the panel's order even when generated in another",
			[]string{"panel-3", "panel-1", "panel-2"},
			pairTasks(2, "panel-1", "panel-2", "panel-3"),
			[]string{"p0-panel-3", "p1-panel-3", "p0-panel-1", "p1-panel-1", "p0-panel-2", "p1-panel-2"}),
		Entry("with models outside the panel after it, in first-seen order",
			[]string{"panel-2"},
			pairTasks(2, "other-b", "panel-2", "other-a"),
			[]string{"p0-panel-2", "p1-panel-2", "p0-other-b", "p1-other-b", "p0-other-a", "p1-other-a"}),
		Entry("in first-seen order for an analyzer without a panel",
			nil,
			pairTasks(2, "embed-b", "embed-a"),
			[]string{"p0-embed-b", "p1-embed-b", "p0-embed-a", "p1-embed-a"}),
		Entry("in generation order when no task names a model",
			nil,
			[]analyzer.Task{modelTask("t2", ""), modelTask("t0", ""), modelTask("t1", "")},
			[]string{"t2", "t0", "t1"}),
	)

	It("leaves the collection expecting the generated order and the analyzer's tasks unchanged", func() {
		generated := pairTasks(2, "panel-1", "panel-2")
		want := taskIDs(generated)
		run(&panelledStub{Stub: newStubWithTasks("relationship", generated), aliases: []string{"panel-1", "panel-2"}})

		Expect(collector.collects).To(HaveLen(1))
		Expect(collector.collects[0].ExpectedIDs).To(Equal(want))
		Expect(taskIDs(collector.collects[0].Tasks)).To(Equal(want))
		Expect(taskIDs(generated)).To(Equal(want))
	})
})
