package main

import (
	"bytes"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
)

var _ = Describe("run status panels", func() {
	stages := []*pb.JobStage{
		{StageName: "classify"},
		{StageName: "relationship", Panel: &pb.PanelProvenance{
			Independence: "independent",
			Models: []*pb.PanelModel{{Alias: "panel-1", ObservedModel: "a", Source: "reply", DeploymentId: "d1"},
				{Alias: "panel-2", Source: "unknown", Fault: "catalog refused"}},
		}},
		{StageName: "requires", Panel: &pb.PanelProvenance{
			Independence: "allowed shared or unknown upstream model", AllowShared: true,
			Models: []*pb.PanelModel{{Alias: "panel-1", ObservedModel: "m", Source: "catalog"}},
		}},
	}

	It("lists only stages with panel provenance, with each alias's model and the allowance", func() {
		var out bytes.Buffer
		writePanelLines(&out, panelsOf(stages))
		Expect(out.String()).To(Equal("Panels:\n" +
			"  relationship  independent; shared-model allowance off; panel-1 → a (reply), panel-2 → unknown (unknown: catalog refused)\n" +
			"  requires  allowed shared or unknown upstream model; shared-model allowance on; panel-1 → m (catalog)\n"))
	})

	It("prints nothing when no stage is a panel", func() {
		var out bytes.Buffer
		writePanelLines(&out, panelsOf(stages[:1]))
		Expect(out.String()).To(BeEmpty())
	})
})
