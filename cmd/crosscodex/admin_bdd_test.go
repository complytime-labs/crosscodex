package main

import (
	"bytes"
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

func (s *stubAdminClient) ScanRetention(_ context.Context, req *connectrpc.Request[pb.ScanRetentionRequest]) (*connectrpc.Response[pb.ScanRetentionResponse], error) {
	s.scanCalls++
	s.scanReq = req.Msg
	if s.scanErr != nil {
		return nil, s.scanErr
	}
	return connectrpc.NewResponse(s.scanResp), nil
}

func (s *stubAdminClient) GetRetentionStats(_ context.Context, req *connectrpc.Request[pb.GetRetentionStatsRequest]) (*connectrpc.Response[pb.GetRetentionStatsResponse], error) {
	s.statsCalls++
	s.statsReq = req.Msg
	if s.statsErr != nil {
		return nil, s.statsErr
	}
	return connectrpc.NewResponse(s.statsResp), nil
}

func (s *stubAdminClient) CreateHold(_ context.Context, req *connectrpc.Request[pb.CreateHoldRequest]) (*connectrpc.Response[pb.CreateHoldResponse], error) {
	s.createCalls++
	s.createReq = req.Msg
	if s.createErr != nil {
		return nil, s.createErr
	}
	return connectrpc.NewResponse(s.createResp), nil
}

func (s *stubAdminClient) ReleaseHold(_ context.Context, req *connectrpc.Request[pb.ReleaseHoldRequest]) (*connectrpc.Response[pb.ReleaseHoldResponse], error) {
	s.releaseCalls++
	s.releaseReq = req.Msg
	if s.releaseErr != nil {
		return nil, s.releaseErr
	}
	return connectrpc.NewResponse(s.releaseResp), nil
}

func (s *stubAdminClient) ListHolds(_ context.Context, req *connectrpc.Request[pb.ListHoldsRequest]) (*connectrpc.Response[pb.ListHoldsResponse], error) {
	s.listCalls++
	s.listReq = req.Msg
	if s.listErr != nil {
		return nil, s.listErr
	}
	return connectrpc.NewResponse(s.listResp), nil
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
	var (
		stdout bytes.Buffer
		stderr bytes.Buffer
	)

	BeforeEach(func() {
		stdout.Reset()
		stderr.Reset()
	})

	Describe("admin retention scan", func() {
		It("renders the retention report counters", func() {
			stub := &stubAdminClient{
				scanResp: &pb.ScanRetentionResponse{
					Report: &pb.RetentionReport{Scanned: 3, Purged: 1},
				},
			}
			root := newAdminTestRoot(&cliState{adminClient: stub})
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"admin", "retention", "scan", "--dry-run", "--tenant", "acme"})

			Expect(root.Execute()).To(Succeed())

			out := stdout.String()
			Expect(out).To(ContainSubstring("scanned"))
			Expect(out).To(ContainSubstring("3"))
			Expect(out).To(ContainSubstring("purged"))
			Expect(out).To(ContainSubstring("1"))

			Expect(stub.scanReq.GetDryRun()).To(BeTrue())
			Expect(stub.scanReq.GetTenantContext().GetTenantId()).To(Equal("acme"))
		})

		It("emits valid JSON with the report fields", func() {
			stub := &stubAdminClient{
				scanResp: &pb.ScanRetentionResponse{
					Report: &pb.RetentionReport{Scanned: 3, Purged: 1},
				},
			}
			root := newAdminTestRoot(&cliState{adminClient: stub})
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"admin", "retention", "scan", "--tenant", "acme", "--json"})

			Expect(root.Execute()).To(Succeed())

			var payload map[string]any
			Expect(json.Unmarshal(stdout.Bytes(), &payload)).To(Succeed())
			report, ok := payload["report"].(map[string]any)
			Expect(ok).To(BeTrue())
			Expect(report["scanned"]).To(BeEquivalentTo(3))
			Expect(report["purged"]).To(BeEquivalentTo(1))
		})

		It("requires --tenant", func() {
			stub := &stubAdminClient{
				scanResp: &pb.ScanRetentionResponse{Report: &pb.RetentionReport{}},
			}
			root := newAdminTestRoot(&cliState{adminClient: stub})
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"admin", "retention", "scan"})

			err := root.Execute()
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
			root := newAdminTestRoot(&cliState{adminClient: stub})
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"admin", "retention", "stats", "--tenant", "acme"})

			Expect(root.Execute()).To(Succeed())

			out := stdout.String()
			Expect(out).To(ContainSubstring("scanned"))
			Expect(out).To(ContainSubstring("7"))
			Expect(out).To(ContainSubstring("Active holds: 4"))
			Expect(stub.statsReq.GetTenantContext().GetTenantId()).To(Equal("acme"))
		})

		It("emits valid JSON with eligible report and active_holds", func() {
			stub := &stubAdminClient{
				statsResp: &pb.GetRetentionStatsResponse{
					Eligible:    &pb.RetentionReport{Scanned: 7, Held: 2},
					ActiveHolds: 4,
				},
			}
			root := newAdminTestRoot(&cliState{adminClient: stub})
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"admin", "retention", "stats", "--tenant", "acme", "--json"})

			Expect(root.Execute()).To(Succeed())

			var payload map[string]any
			Expect(json.Unmarshal(stdout.Bytes(), &payload)).To(Succeed())
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
			root := newAdminTestRoot(&cliState{adminClient: stub})
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{
				"admin", "hold", "create",
				"--tenant", "acme",
				"--name", "litigation",
				"--job", "job_9",
				"--created-before", "2026-01-02T15:04:05Z",
				"--expires-at", "2026-12-31T00:00:00Z",
			})

			Expect(root.Execute()).To(Succeed())

			Expect(stub.createReq.GetName()).To(Equal("litigation"))
			Expect(stub.createReq.GetTenantContext().GetTenantId()).To(Equal("acme"))
			Expect(stub.createReq.GetScope().GetJobId()).To(Equal("job_9"))
			Expect(stub.createReq.GetScope().GetCreatedBefore()).NotTo(BeNil())
			Expect(stub.createReq.GetExpiresAt()).NotTo(BeNil())

			out := stdout.String()
			Expect(out).To(ContainSubstring("hold_123"))
			Expect(out).To(ContainSubstring("litigation"))
		})

		It("leaves optional timestamps unset when the flags are omitted", func() {
			stub := &stubAdminClient{
				createResp: &pb.CreateHoldResponse{
					Hold: &pb.Hold{Id: "hold_123", Name: "litigation"},
				},
			}
			root := newAdminTestRoot(&cliState{adminClient: stub})
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"admin", "hold", "create", "--tenant", "acme", "--name", "litigation"})

			Expect(root.Execute()).To(Succeed())

			Expect(stub.createReq.GetScope().GetCreatedBefore()).To(BeNil())
			Expect(stub.createReq.GetExpiresAt()).To(BeNil())
		})

		It("emits valid JSON with the created hold", func() {
			stub := &stubAdminClient{
				createResp: &pb.CreateHoldResponse{
					Hold: &pb.Hold{Id: "hold_123", Name: "litigation"},
				},
			}
			root := newAdminTestRoot(&cliState{adminClient: stub})
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"admin", "hold", "create", "--tenant", "acme", "--name", "litigation", "--json"})

			Expect(root.Execute()).To(Succeed())

			var payload map[string]any
			Expect(json.Unmarshal(stdout.Bytes(), &payload)).To(Succeed())
			Expect(payload["id"]).To(Equal("hold_123"))
			Expect(payload["name"]).To(Equal("litigation"))
		})

		It("rejects a malformed --created-before without calling the RPC", func() {
			stub := &stubAdminClient{
				createResp: &pb.CreateHoldResponse{Hold: &pb.Hold{}},
			}
			root := newAdminTestRoot(&cliState{adminClient: stub})
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"admin", "hold", "create", "--tenant", "acme", "--name", "x", "--created-before", "not-a-time"})

			err := root.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("created-before"))
			Expect(stub.createCalls).To(Equal(0))
		})

		It("rejects a malformed --expires-at without calling the RPC", func() {
			stub := &stubAdminClient{
				createResp: &pb.CreateHoldResponse{Hold: &pb.Hold{}},
			}
			root := newAdminTestRoot(&cliState{adminClient: stub})
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"admin", "hold", "create", "--tenant", "acme", "--name", "x", "--expires-at", "not-a-time"})

			err := root.Execute()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("expires-at"))
			Expect(stub.createCalls).To(Equal(0))
		})
	})

	Describe("admin hold release", func() {
		It("sends name and tenant and renders a confirmation", func() {
			stub := &stubAdminClient{releaseResp: &pb.ReleaseHoldResponse{}}
			root := newAdminTestRoot(&cliState{adminClient: stub})
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"admin", "hold", "release", "--tenant", "acme", "--name", "litigation"})

			Expect(root.Execute()).To(Succeed())

			Expect(stub.releaseReq.GetName()).To(Equal("litigation"))
			Expect(stub.releaseReq.GetTenantContext().GetTenantId()).To(Equal("acme"))
			Expect(stdout.String()).To(ContainSubstring("litigation"))
			Expect(stdout.String()).To(ContainSubstring("released"))
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
			root := newAdminTestRoot(&cliState{adminClient: stub})
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"admin", "hold", "list", "--tenant", "acme"})

			Expect(root.Execute()).To(Succeed())

			out := stdout.String()
			Expect(out).To(ContainSubstring("hold_1"))
			Expect(out).To(ContainSubstring("alpha"))
			Expect(out).To(ContainSubstring("hold_2"))
			Expect(out).To(ContainSubstring("beta"))
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
			root := newAdminTestRoot(&cliState{adminClient: stub})
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"admin", "hold", "list", "--tenant", "acme", "--json"})

			Expect(root.Execute()).To(Succeed())

			var payload []map[string]any
			Expect(json.Unmarshal(stdout.Bytes(), &payload)).To(Succeed())
			Expect(payload).To(HaveLen(2))
			Expect(payload[0]["id"]).To(Equal("hold_1"))
			Expect(payload[1]["name"]).To(Equal("beta"))
		})

		It("reports no holds and emits an empty JSON array when empty", func() {
			stub := &stubAdminClient{listResp: &pb.ListHoldsResponse{}}
			root := newAdminTestRoot(&cliState{adminClient: stub})
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{"admin", "hold", "list", "--tenant", "acme"})

			Expect(root.Execute()).To(Succeed())
			Expect(stdout.String()).To(ContainSubstring("No holds found"))

			stdout.Reset()
			jsonStub := &stubAdminClient{listResp: &pb.ListHoldsResponse{}}
			jsonRoot := newAdminTestRoot(&cliState{adminClient: jsonStub})
			jsonRoot.SetOut(&stdout)
			jsonRoot.SetErr(&stderr)
			jsonRoot.SetArgs([]string{"admin", "hold", "list", "--tenant", "acme", "--json"})

			Expect(jsonRoot.Execute()).To(Succeed())
			var payload []any
			Expect(json.Unmarshal(stdout.Bytes(), &payload)).To(Succeed())
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
				root := newAdminTestRoot(&cliState{adminClient: stub})
				root.SetOut(&stdout)
				root.SetErr(&stderr)
				root.SetArgs(args)

				err := root.Execute()
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
