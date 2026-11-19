//go:build !integration

package main

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	crosscodexv1connect "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	intcleaning "github.com/complytime-labs/crosscodex/internal/cleaning"
	"github.com/complytime-labs/crosscodex/internal/gateway"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/cleaning/cleaningtest"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
	"github.com/complytime-labs/crosscodex/pkg/jobconfig"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// stageRecordClient answers GetCurrentStageRecord and records what it was
// sent; every other PipelineServiceClient method panics (nil embed).
type stageRecordClient struct {
	crosscodexv1connect.PipelineServiceClient
	resp *pb.GetCurrentStageRecordResponse
	err  error

	gotTenant string
	gotReq    *pb.GetCurrentStageRecordRequest
}

func (c *stageRecordClient) GetCurrentStageRecord(ctx context.Context, req *connect.Request[pb.GetCurrentStageRecordRequest]) (*connect.Response[pb.GetCurrentStageRecordResponse], error) {
	c.gotTenant, _ = tenant.FromContext(ctx)
	c.gotReq = req.Msg
	if c.err != nil {
		return nil, c.err
	}
	return connect.NewResponse(c.resp), nil
}

// On the graph role the cleaning engine reads the current stage record from
// the pipeline role. Any failure to read it must fail the pass closed: an
// unknown record treated as "not found" would let a stale verdict pass as
// current.
var _ = Describe("remoteStageRecord", func() {
	const tenantID = "crosscodexd-test"

	It("asks for the stage in the tenant's context and returns the record", func() {
		client := &stageRecordClient{resp: &pb.GetCurrentStageRecordResponse{
			Found: true, PromptName: "mapping", PromptVersion: "v3", Models: map[string]string{"a": "rev-1"},
		}}

		rec, found, err := remoteStageRecord(client)(context.Background(), tenantID, "analyzer")

		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(rec).To(Equal(cleaning.StageRecord{PromptName: "mapping", PromptVersion: "v3", Models: map[string]string{"a": "rev-1"}}))
		Expect(client.gotTenant).To(Equal(tenantID))
		Expect(client.gotReq.GetTenantContext().GetTenantId()).To(Equal(tenantID))
		Expect(client.gotReq.GetStage()).To(Equal("analyzer"))
	})

	It("returns not found, and no error, when the pipeline has no record", func() {
		client := &stageRecordClient{resp: &pb.GetCurrentStageRecordResponse{Found: false}}

		rec, found, err := remoteStageRecord(client)(context.Background(), tenantID, "analyzer")

		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeFalse())
		Expect(rec).To(Equal(cleaning.StageRecord{}))
	})

	It("refuses an invalid tenant before calling the pipeline", func() {
		client := &stageRecordClient{resp: &pb.GetCurrentStageRecordResponse{}}

		_, found, err := remoteStageRecord(client)(context.Background(), "Not A Tenant!", "analyzer")

		Expect(err).To(HaveOccurred())
		Expect(found).To(BeFalse())
		Expect(client.gotReq).To(BeNil())
	})

	// The pipeline's code and message are the pipeline's answer to the graph
	// role, not to the API caller: passed through, a PermissionDenied for the
	// graph role's certificate would read as the caller's own, and a dial error
	// would name an internal address (CWE-209).
	DescribeTable("fails closed on an RPC error as Unavailable, naming pipeline.endpoint and withholding the cause",
		func(code connect.Code) {
			cause := connect.NewError(code, errors.New("dial tcp 10.9.8.7:8443: internal detail"))
			client := &stageRecordClient{err: cause}

			_, found, err := remoteStageRecord(client)(context.Background(), tenantID, "analyzer")

			Expect(found).To(BeFalse())
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeUnavailable))
			Expect(err).To(MatchError(And(ContainSubstring("pipeline.endpoint"), ContainSubstring("tls.service_identities"))))
			Expect(err).NotTo(MatchError(cause))
			Expect(err.Error()).NotTo(ContainSubstring("10.9.8.7"))
			Expect(err.Error()).NotTo(ContainSubstring("internal detail"))
		},
		Entry("unavailable", connect.CodeUnavailable),
		Entry("permission denied", connect.CodePermissionDenied),
		Entry("internal", connect.CodeInternal),
	)

	Context("against an unreachable pipeline role", func() {
		var (
			current      cleaning.CurrentFunc
			pipelineAddr string
		)

		BeforeEach(func() {
			pipelineAddr = unusedAddr()
			tlsCfg := config.TLSConfig{
				Mode: "mutual",
				CA:   testTLSWiringCAPath,
				Cert: testTLSWiringClientCertPath,
				Key:  testTLSWiringClientKeyPath,
			}
			client, err := gateway.NewPipelineClient(context.Background(), pipelineAddr, tlsCfg)
			Expect(err).NotTo(HaveOccurred())
			current = remoteStageRecord(client)
		})

		It("returns an error naming pipeline.endpoint", func() {
			_, found, err := current(context.Background(), tenantID, "analyzer")
			Expect(err).To(MatchError(ContainSubstring("current stage record via pipeline.endpoint: ")))
			Expect(err.Error()).NotTo(ContainSubstring(pipelineAddr))
			Expect(found).To(BeFalse())
		})

		// The cleaning handler as bootstrap builds it for the graph role: the
		// caller gets Unavailable naming pipeline.endpoint, never the dial
		// address, and nothing is written.
		It("fails a cleaning apply through the handler as Unavailable, writing nothing", func() {
			ctx, err := tenant.WithTenant(context.Background(), tenantID)
			Expect(err).NotTo(HaveOccurred())
			ctx = authn.WithIdentity(ctx, &authn.Identity{Subject: "operator", TenantID: tenantID, Roles: []string{authn.RoleAdmin}})
			g := memdriver.New()
			Expect(g.CreateGraph(ctx, tenantID)).To(Succeed())
			store := &cleaningtest.Fake{Jobs: map[string][]cleaning.Job{tenantID: {{
				ID:          "job-1",
				CompletedAt: time.Now(),
				Catalogs:    jobconfig.Catalogs{SourceID: "cat-a", TargetID: "cat-b"},
				Stages:      map[string]*cleaning.StageRecord{"analyzer": {PromptName: "mapping", PromptVersion: "v1"}},
			}}}}
			handler := intcleaning.NewService(cleaning.NewEngine(store, g, &config.CleaningConfig{}, current, nil))

			_, err = handler.RunCleaning(ctx, connect.NewRequest(&pb.RunCleaningRequest{
				TenantContext: &pb.TenantContext{TenantId: tenantID},
				Mode:          pb.CleaningMode_CLEANING_MODE_APPLY,
				ExpectToken:   "any-token",
			}))

			// connect-go sends the code and message of the first *connect.Error
			// in the chain (errors.As), dropping wrapper text.
			var sent *connect.Error
			Expect(errors.As(err, &sent)).To(BeTrue())
			Expect(sent.Code()).To(Equal(connect.CodeUnavailable))
			Expect(sent.Message()).To(ContainSubstring("pipeline.endpoint"))
			Expect(sent.Message()).NotTo(ContainSubstring(pipelineAddr))
			Expect(err.Error()).NotTo(ContainSubstring(pipelineAddr))
			Expect(store.Runs(tenantID)).To(BeEmpty())
			findings, _, err := store.ListFindings(ctx, tenantID, cleaning.FindingFilter{PageSize: 100})
			Expect(err).NotTo(HaveOccurred())
			Expect(findings).To(BeEmpty())
		})
	})
})
