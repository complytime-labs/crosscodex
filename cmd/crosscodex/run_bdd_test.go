package main

import (
	"context"
	"encoding/json"
	"errors"

	connectrpc "connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
)

// fakeStartAnalysisClient records StartAnalysis requests; other
// GatewayServiceClient methods panic through the nil embed.
type fakeStartAnalysisClient struct {
	crosscodexv1connect.GatewayServiceClient
	calls []*pb.StartAnalysisRequest
	err   error
}

func (f *fakeStartAnalysisClient) StartAnalysis(_ context.Context, req *connectrpc.Request[pb.StartAnalysisRequest]) (*connectrpc.Response[pb.StartAnalysisResponse], error) {
	f.calls = append(f.calls, req.Msg)
	if f.err != nil {
		return nil, f.err
	}
	return connectrpc.NewResponse(&pb.StartAnalysisResponse{JobId: "job-1", Status: pb.JobStatus_JOB_STATUS_PENDING}), nil
}

var _ = Describe("run start", func() {
	var fake *fakeStartAnalysisClient

	newCmd := func() *cobra.Command {
		cmd := newRunCmd(&cliState{client: fake})
		cmd.PersistentFlags().Bool("json", false, "output as JSON") // root-level in production
		return cmd
	}

	BeforeEach(func() { fake = &fakeStartAnalysisClient{} })

	It("starts analysis of the named catalog and prints the job", func() {
		out, _, err := execute(newCmd(), "start", "--source", "cat-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.calls).To(HaveLen(1))
		Expect(fake.calls[0].GetSourceCatalogId()).To(Equal("cat-1"))
		Expect(fake.calls[0].GetTenantContext()).To(BeNil())
		Expect(out).To(ContainSubstring("Job ID: job-1"))
		Expect(out).To(ContainSubstring("Status: " + formatJobStatus(pb.JobStatus_JOB_STATUS_PENDING)))
	})

	It("passes --tenant as the request's tenant context", func() {
		_, _, err := execute(newCmd(), "start", "--source", "cat-1", "--tenant", "acme")
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.calls[0].GetTenantContext().GetTenantId()).To(Equal("acme"))
	})

	It("emits job_id and status as JSON", func() {
		out, _, err := execute(newCmd(), "start", "--source", "cat-1", "--json")
		Expect(err).NotTo(HaveOccurred())
		var got map[string]string
		Expect(json.Unmarshal([]byte(out), &got)).To(Succeed())
		Expect(got).To(Equal(map[string]string{"job_id": "job-1", "status": "JOB_STATUS_PENDING"}))
	})

	It("requires --source and says where to find catalog IDs", func() {
		_, _, err := execute(newCmd(), "start")
		Expect(err).To(MatchError(And(ContainSubstring("--source"), ContainSubstring("crosscodex catalog list"))))
		Expect(fake.calls).To(BeEmpty())
	})

	It("refuses a file argument and points at catalog import", func() {
		_, _, err := execute(newCmd(), "start", "nist.json")
		Expect(err).To(MatchError(ContainSubstring("crosscodex catalog import <file>")))
		Expect(fake.calls).To(BeEmpty())
	})

	It("wraps an RPC failure and preserves its code", func() {
		fake.err = connectrpc.NewError(connectrpc.CodeNotFound, errors.New("catalog \"cat-x\" not found"))
		_, _, err := execute(newCmd(), "start", "--source", "cat-x")
		Expect(err).To(MatchError(And(ContainSubstring("failed to start analysis"), ContainSubstring("not found"))))
		Expect(connectrpc.CodeOf(err)).To(Equal(connectrpc.CodeNotFound))
		Expect(fake.calls).To(HaveLen(1))
	})
})
