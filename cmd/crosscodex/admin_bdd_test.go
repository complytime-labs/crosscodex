package main

import (
	"context"
	"encoding/json"

	connectrpc "connectrpc.com/connect"
	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1/crosscodexv1connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/spf13/cobra"
)

// stubAdminClient implements crosscodexv1connect.AdminServiceClient. Only the
// methods exercised by a test are overridden; the embedded interface is nil, so
// any unimplemented method panics if called (none are, in these tests). Each
// method records the request it received and the number of times it was called
// so specs can assert on the wire payload and on non-invocation.
type stubAdminClient struct {
	crosscodexv1connect.AdminServiceClient

	scanReq   *pb.ScanRetentionRequest
	scanResp  *pb.ScanRetentionResponse
	scanErr   error
	scanCalls int

	statsReq   *pb.GetRetentionStatsRequest
	statsResp  *pb.GetRetentionStatsResponse
	statsErr   error
	statsCalls int

	createReq   *pb.CreateHoldRequest
	createResp  *pb.CreateHoldResponse
	createErr   error
	createCalls int

	releaseReq   *pb.ReleaseHoldRequest
	releaseResp  *pb.ReleaseHoldResponse
	releaseErr   error
	releaseCalls int

	listReq   *pb.ListHoldsRequest
	listResp  *pb.ListHoldsResponse
	listErr   error
	listCalls int
}

// stubRPC is the shared body of every stubAdminClient method: it counts the
// call, records the request message, then returns err if set, else resp.
func stubRPC[Req, Resp any](calls *int, got **Req, err error, resp *Resp, req *connectrpc.Request[Req]) (*connectrpc.Response[Resp], error) {
	*calls++
	*got = req.Msg
	if err != nil {
		return nil, err
	}
	return connectrpc.NewResponse(resp), nil
}

func (s *stubAdminClient) ScanRetention(_ context.Context, req *connectrpc.Request[pb.ScanRetentionRequest]) (*connectrpc.Response[pb.ScanRetentionResponse], error) {
	return stubRPC(&s.scanCalls, &s.scanReq, s.scanErr, s.scanResp, req)
}

func (s *stubAdminClient) GetRetentionStats(_ context.Context, req *connectrpc.Request[pb.GetRetentionStatsRequest]) (*connectrpc.Response[pb.GetRetentionStatsResponse], error) {
	return stubRPC(&s.statsCalls, &s.statsReq, s.statsErr, s.statsResp, req)
}

func (s *stubAdminClient) CreateHold(_ context.Context, req *connectrpc.Request[pb.CreateHoldRequest]) (*connectrpc.Response[pb.CreateHoldResponse], error) {
	return stubRPC(&s.createCalls, &s.createReq, s.createErr, s.createResp, req)
}

func (s *stubAdminClient) ReleaseHold(_ context.Context, req *connectrpc.Request[pb.ReleaseHoldRequest]) (*connectrpc.Response[pb.ReleaseHoldResponse], error) {
	return stubRPC(&s.releaseCalls, &s.releaseReq, s.releaseErr, s.releaseResp, req)
}

func (s *stubAdminClient) ListHolds(_ context.Context, req *connectrpc.Request[pb.ListHoldsRequest]) (*connectrpc.Response[pb.ListHoldsResponse], error) {
	return stubRPC(&s.listCalls, &s.listReq, s.listErr, s.listResp, req)
}

// newAdminTestRoot wires newAdminCmd under a minimal root that provides the
// same output-mode persistent flags the real root defines, so emit() behaves
// identically without dialing a daemon.
func newAdminTestRoot(state *cliState) *cobra.Command {
	root := &cobra.Command{Use: "crosscodex"}
	root.PersistentFlags().Bool("json", false, "")
	root.PersistentFlags().Bool("plain", false, "")
	root.PersistentFlags().Bool("no-color", false, "")
	root.AddCommand(newAdminCmd(state))
	return root
}

var _ = Describe("Admin Commands", func() {
	Describe("admin retention scan", func() {
		It("renders the retention report counters", func() {
			stub := &stubAdminClient{
				scanResp: &pb.ScanRetentionResponse{
					Report: &pb.RetentionReport{Scanned: 3, Purged: 1},
				},
			}
			stdout, _, err := execute(newAdminTestRoot(&cliState{adminClient: stub}), "admin", "retention", "scan", "--dry-run", "--tenant", "acme")
			Expect(err).NotTo(HaveOccurred())

			Expect(stdout).To(ContainSubstring("scanned"))
			Expect(stdout).To(ContainSubstring("3"))
			Expect(stdout).To(ContainSubstring("purged"))
			Expect(stdout).To(ContainSubstring("1"))

			Expect(stub.scanReq.GetDryRun()).To(BeTrue())
			Expect(stub.scanReq.GetTenantContext().GetTenantId()).To(Equal("acme"))
		})

		It("emits valid JSON with the report fields", func() {
			stub := &stubAdminClient{
				scanResp: &pb.ScanRetentionResponse{
					Report: &pb.RetentionReport{Scanned: 3, Purged: 1},
				},
			}
			stdout, _, err := execute(newAdminTestRoot(&cliState{adminClient: stub}), "admin", "retention", "scan", "--tenant", "acme", "--json")
			Expect(err).NotTo(HaveOccurred())

			var payload map[string]any
			Expect(json.Unmarshal([]byte(stdout), &payload)).To(Succeed())
			report, ok := payload["report"].(map[string]any)
			Expect(ok).To(BeTrue())
			Expect(report["scanned"]).To(BeEquivalentTo(3))
			Expect(report["purged"]).To(BeEquivalentTo(1))
		})

		It("requires --tenant", func() {
			stub := &stubAdminClient{
				scanResp: &pb.ScanRetentionResponse{Report: &pb.RetentionReport{}},
			}
			_, _, err := execute(newAdminTestRoot(&cliState{adminClient: stub}), "admin", "retention", "scan")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("tenant"))
		})
	})

	Describe("admin retention stats", func() {
		It("renders the eligible report counters and active-holds count", func() {
			stub := &stubAdminClient{
				statsResp: &pb.GetRetentionStatsResponse{
					Eligible:    &pb.RetentionReport{Scanned: 7, Held: 2},
					ActiveHolds: 4,
				},
			}
			stdout, _, err := execute(newAdminTestRoot(&cliState{adminClient: stub}), "admin", "retention", "stats", "--tenant", "acme")
			Expect(err).NotTo(HaveOccurred())

			Expect(stdout).To(ContainSubstring("scanned"))
			Expect(stdout).To(ContainSubstring("7"))
			Expect(stdout).To(ContainSubstring("Active holds: 4"))
			Expect(stub.statsReq.GetTenantContext().GetTenantId()).To(Equal("acme"))
		})

		It("emits valid JSON with eligible report and active_holds", func() {
			stub := &stubAdminClient{
				statsResp: &pb.GetRetentionStatsResponse{
					Eligible:    &pb.RetentionReport{Scanned: 7, Held: 2},
					ActiveHolds: 4,
				},
			}
			stdout, _, err := execute(newAdminTestRoot(&cliState{adminClient: stub}), "admin", "retention", "stats", "--tenant", "acme", "--json")
			Expect(err).NotTo(HaveOccurred())

			var payload map[string]any
			Expect(json.Unmarshal([]byte(stdout), &payload)).To(Succeed())
			Expect(payload["active_holds"]).To(BeEquivalentTo(4))
			eligible, ok := payload["eligible"].(map[string]any)
			Expect(ok).To(BeTrue())
			Expect(eligible["scanned"]).To(BeEquivalentTo(7))
			Expect(eligible["held"]).To(BeEquivalentTo(2))
		})
	})

	Describe("admin hold create", func() {
		It("sends name, tenant, and full scope with optional timestamps", func() {
			stub := &stubAdminClient{
				createResp: &pb.CreateHoldResponse{
					Hold: &pb.Hold{Id: "hold_123", Name: "litigation"},
				},
			}
			stdout, _, err := execute(newAdminTestRoot(&cliState{adminClient: stub}),
				"admin", "hold", "create",
				"--tenant", "acme",
				"--name", "litigation",
				"--job", "job_9",
				"--created-before", "2026-01-02T15:04:05Z",
				"--expires-at", "2026-12-31T00:00:00Z",
			)
			Expect(err).NotTo(HaveOccurred())

			Expect(stub.createReq.GetName()).To(Equal("litigation"))
			Expect(stub.createReq.GetTenantContext().GetTenantId()).To(Equal("acme"))
			Expect(stub.createReq.GetScope().GetJobId()).To(Equal("job_9"))
			Expect(stub.createReq.GetScope().GetCreatedBefore()).NotTo(BeNil())
			Expect(stub.createReq.GetExpiresAt()).NotTo(BeNil())

			Expect(stdout).To(ContainSubstring("hold_123"))
			Expect(stdout).To(ContainSubstring("litigation"))
		})

		It("leaves optional timestamps unset when the flags are omitted", func() {
			stub := &stubAdminClient{
				createResp: &pb.CreateHoldResponse{
					Hold: &pb.Hold{Id: "hold_123", Name: "litigation"},
				},
			}
			_, _, err := execute(newAdminTestRoot(&cliState{adminClient: stub}), "admin", "hold", "create", "--tenant", "acme", "--name", "litigation")
			Expect(err).NotTo(HaveOccurred())

			Expect(stub.createReq.GetScope().GetCreatedBefore()).To(BeNil())
			Expect(stub.createReq.GetExpiresAt()).To(BeNil())
		})

		It("emits valid JSON with the created hold", func() {
			stub := &stubAdminClient{
				createResp: &pb.CreateHoldResponse{
					Hold: &pb.Hold{Id: "hold_123", Name: "litigation"},
				},
			}
			stdout, _, err := execute(newAdminTestRoot(&cliState{adminClient: stub}), "admin", "hold", "create", "--tenant", "acme", "--name", "litigation", "--json")
			Expect(err).NotTo(HaveOccurred())

			var payload map[string]any
			Expect(json.Unmarshal([]byte(stdout), &payload)).To(Succeed())
			Expect(payload["id"]).To(Equal("hold_123"))
			Expect(payload["name"]).To(Equal("litigation"))
		})

		It("rejects a malformed --created-before without calling the RPC", func() {
			stub := &stubAdminClient{
				createResp: &pb.CreateHoldResponse{Hold: &pb.Hold{}},
			}
			_, _, err := execute(newAdminTestRoot(&cliState{adminClient: stub}), "admin", "hold", "create", "--tenant", "acme", "--name", "x", "--created-before", "not-a-time")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("created-before"))
			Expect(stub.createCalls).To(Equal(0))
		})

		It("rejects a malformed --expires-at without calling the RPC", func() {
			stub := &stubAdminClient{
				createResp: &pb.CreateHoldResponse{Hold: &pb.Hold{}},
			}
			_, _, err := execute(newAdminTestRoot(&cliState{adminClient: stub}), "admin", "hold", "create", "--tenant", "acme", "--name", "x", "--expires-at", "not-a-time")
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("expires-at"))
			Expect(stub.createCalls).To(Equal(0))
		})
	})

	Describe("admin hold release", func() {
		It("sends name and tenant and renders a confirmation", func() {
			stub := &stubAdminClient{releaseResp: &pb.ReleaseHoldResponse{}}
			stdout, _, err := execute(newAdminTestRoot(&cliState{adminClient: stub}), "admin", "hold", "release", "--tenant", "acme", "--name", "litigation")
			Expect(err).NotTo(HaveOccurred())

			Expect(stub.releaseReq.GetName()).To(Equal("litigation"))
			Expect(stub.releaseReq.GetTenantContext().GetTenantId()).To(Equal("acme"))
			Expect(stdout).To(ContainSubstring("litigation"))
			Expect(stdout).To(ContainSubstring("released"))
		})
	})

	Describe("admin hold list", func() {
		It("renders a table of holds", func() {
			stub := &stubAdminClient{
				listResp: &pb.ListHoldsResponse{
					Holds: []*pb.Hold{
						{Id: "hold_1", Name: "alpha", Scope: &pb.HoldScope{JobId: "job_a"}},
						{Id: "hold_2", Name: "beta", Scope: &pb.HoldScope{JobId: "job_b"}},
					},
				},
			}
			stdout, _, err := execute(newAdminTestRoot(&cliState{adminClient: stub}), "admin", "hold", "list", "--tenant", "acme")
			Expect(err).NotTo(HaveOccurred())

			Expect(stdout).To(ContainSubstring("hold_1"))
			Expect(stdout).To(ContainSubstring("alpha"))
			Expect(stdout).To(ContainSubstring("hold_2"))
			Expect(stdout).To(ContainSubstring("beta"))
			Expect(stub.listReq.GetTenantContext().GetTenantId()).To(Equal("acme"))
		})

		It("emits valid JSON for holds", func() {
			stub := &stubAdminClient{
				listResp: &pb.ListHoldsResponse{
					Holds: []*pb.Hold{
						{Id: "hold_1", Name: "alpha"},
						{Id: "hold_2", Name: "beta"},
					},
				},
			}
			stdout, _, err := execute(newAdminTestRoot(&cliState{adminClient: stub}), "admin", "hold", "list", "--tenant", "acme", "--json")
			Expect(err).NotTo(HaveOccurred())

			var payload []map[string]any
			Expect(json.Unmarshal([]byte(stdout), &payload)).To(Succeed())
			Expect(payload).To(HaveLen(2))
			Expect(payload[0]["id"]).To(Equal("hold_1"))
			Expect(payload[1]["name"]).To(Equal("beta"))
		})

		It("reports no holds and emits an empty JSON array when empty", func() {
			stub := &stubAdminClient{listResp: &pb.ListHoldsResponse{}}
			stdout, _, err := execute(newAdminTestRoot(&cliState{adminClient: stub}), "admin", "hold", "list", "--tenant", "acme")
			Expect(err).NotTo(HaveOccurred())
			Expect(stdout).To(ContainSubstring("No holds found"))

			jsonStub := &stubAdminClient{listResp: &pb.ListHoldsResponse{}}
			jsonRoot := newAdminTestRoot(&cliState{adminClient: jsonStub})

			stdout, _, err = execute(jsonRoot, "admin", "hold", "list", "--tenant", "acme", "--json")
			Expect(err).NotTo(HaveOccurred())
			var payload []any
			Expect(json.Unmarshal([]byte(stdout), &payload)).To(Succeed())
			Expect(payload).To(BeEmpty())
		})
	})

	Describe("tenant requirement", func() {
		DescribeTable("errors and does not call the RPC when --tenant is missing",
			func(args []string, calls func(*stubAdminClient) int) {
				stub := &stubAdminClient{
					scanResp:    &pb.ScanRetentionResponse{Report: &pb.RetentionReport{}},
					statsResp:   &pb.GetRetentionStatsResponse{Eligible: &pb.RetentionReport{}},
					createResp:  &pb.CreateHoldResponse{Hold: &pb.Hold{}},
					releaseResp: &pb.ReleaseHoldResponse{},
					listResp:    &pb.ListHoldsResponse{},
				}
				_, _, err := execute(newAdminTestRoot(&cliState{adminClient: stub}), args...)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("tenant"))
				Expect(calls(stub)).To(Equal(0))
			},
			Entry("retention scan", []string{"admin", "retention", "scan"}, func(s *stubAdminClient) int { return s.scanCalls }),
			Entry("retention stats", []string{"admin", "retention", "stats"}, func(s *stubAdminClient) int { return s.statsCalls }),
			Entry("hold create", []string{"admin", "hold", "create", "--name", "x"}, func(s *stubAdminClient) int { return s.createCalls }),
			Entry("hold release", []string{"admin", "hold", "release", "--name", "x"}, func(s *stubAdminClient) int { return s.releaseCalls }),
			Entry("hold list", []string{"admin", "hold", "list"}, func(s *stubAdminClient) int { return s.listCalls }),
		)
	})

	Describe("registration", func() {
		It("registers the admin subtree under the real root", func() {
			root := newRootCmd()
			leaf, _, err := root.Find([]string{"admin", "retention", "scan"})
			Expect(err).NotTo(HaveOccurred())
			Expect(leaf.Name()).To(Equal("scan"))

			hold, _, err := root.Find([]string{"admin", "hold", "create"})
			Expect(err).NotTo(HaveOccurred())
			Expect(hold.Name()).To(Equal("create"))
		})
	})
})
