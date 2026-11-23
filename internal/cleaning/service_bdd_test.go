package cleaning_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	cleaningsvc "github.com/complytime-labs/crosscodex/internal/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/results"
	"github.com/complytime-labs/crosscodex/pkg/attestation"
	"github.com/complytime-labs/crosscodex/pkg/authn"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/cleaning/cleaningtest"
	"github.com/complytime-labs/crosscodex/pkg/config"
	"github.com/complytime-labs/crosscodex/pkg/graphdb"
	"github.com/complytime-labs/crosscodex/pkg/graphdb/memdriver"
	"github.com/complytime-labs/crosscodex/pkg/jobconfig"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

const (
	tenantA  = "tenant-a"
	tenantB  = "tenant-b"
	reviewer = "rev-1"

	decideDenied = "recording a decision needs the reviewer role. Ask an operator to grant it to your " +
		"client certificate through auth.x509_mappings. An admin alone cannot decide findings"
)

var (
	fixtureTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fixtureNow  = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

	// Controls use SP 800-53 (cat-a) and SP 800-171r3 (cat-b) identifiers;
	// the handler never reads their format.
	a1 = control("a1", "cat-a", "AC-2", "Account Management")
	a2 = control("a2", "cat-a", "AC-3", "Access Enforcement")
	b1 = control("b1", "cat-b", "03.01.01", "Account Management")
	b2 = control("b2", "cat-b", "03.01.02", "Access Enforcement")

	// job j1 maps cat-a onto cat-b with both catalogs' names recorded.
	job = cleaning.Job{ID: "j1", CompletedAt: fixtureTime, Catalogs: jobconfig.Catalogs{
		SourceID: "cat-a", TargetID: "cat-b",
		SourceLabel: jobconfig.Label{Name: "NIST SP 800-53", Version: "5.1.1", Recorded: true},
		TargetLabel: jobconfig.Label{Name: "NIST SP 800-171", Version: "r3", Recorded: true},
	}}

	a1b1     = cleaning.VerdictKey{JobID: "j1", SourceID: "a1", TargetID: "b1"}
	a1b2     = cleaning.VerdictKey{JobID: "j1", SourceID: "a1", TargetID: "b2"}
	a2b1     = cleaning.VerdictKey{JobID: "j1", SourceID: "a2", TargetID: "b1"}
	fanout   = cleaning.FindingID(cleaning.Finding{Signal: cleaning.SignalEquivalenceFanout, Concerns: []cleaning.VerdictKey{a1b1, a1b2}})
	conflict = cleaning.FindingID(cleaning.Finding{Signal: cleaning.SignalConflictsWith, Concerns: []cleaning.VerdictKey{a2b1}})
)

func control(id, catalog, identifier, title string) graphdb.Node {
	return graphdb.Node{ID: id, Label: "Control", ValidFrom: fixtureTime, Properties: map[string]any{
		"identifier": identifier, "label": identifier, "title": title, "catalog_id": catalog,
	}}
}

// fixtureGraph holds job j1's verdicts in tenant A: a1 is EQUIVALENT to b1
// and b2 (an equivalence fanout the policy queues for review) and a2
// CONFLICTS_WITH b1 (a finding the policy retires).
func fixtureGraph() graphdb.GraphDB {
	ctx := tenantCtx(tenantA)
	g := memdriver.New()
	Expect(g.CreateGraph(ctx, tenantA)).To(Succeed())
	for _, n := range []graphdb.Node{a1, a2, b1, b2} {
		Expect(g.CreateNode(ctx, tenantA, n)).To(Succeed())
	}
	for _, e := range []struct{ id, src, tgt, typ string }{
		{"e-a1-b1", "a1", "b1", "EQUIVALENT"},
		{"e-a1-b2", "a1", "b2", "EQUIVALENT"},
		{"e-a2-b1", "a2", "b1", "CONFLICTS_WITH"},
	} {
		Expect(g.CreateEdge(ctx, tenantA, e.src, e.tgt, graphdb.Edge{
			ID: e.id, Label: results.SemanticMatchEdgeLabel, ValidFrom: fixtureTime,
			Properties: map[string]any{"job_id": "j1", "relationship_type": e.typ},
		})).To(Succeed())
	}
	return g
}

func fixturePolicy() *config.CleaningConfig {
	report := config.SignalPolicy{Action: config.CleaningActionReport}
	return &config.CleaningConfig{MaxRetirePerRun: 1000, Signals: config.CleaningSignals{
		EquivalenceFanout:     config.SignalPolicy{Action: config.CleaningActionHuman},
		CrossJobContradiction: report,
		LowAgreement:          config.SignalPolicy{Action: config.CleaningActionReport, MinAgreementFraction: 0.5},
		ConflictsWith:         config.SignalPolicy{Action: config.CleaningActionRetire},
		StalePrompt:           report, StaleModel: report,
	}}
}

// fixtureStore registers reviewer as active in both tenants, so a refusal
// in tenant B is never the reviewer check.
func fixtureStore() *cleaningtest.Fake {
	active := map[string]string{reviewer: cleaningtest.ReviewerActive}
	return &cleaningtest.Fake{
		Jobs:      map[string][]cleaning.Job{tenantA: {job}},
		Reviewers: map[string]map[string]string{tenantA: active, tenantB: active},
		Now:       func() time.Time { return fixtureNow },
	}
}

func newSigner() attestation.Generator {
	kp, err := attestation.NewEphemeralKeyProvider()
	Expect(err).NotTo(HaveOccurred())
	signer, err := attestation.NewGenerator(kp)
	Expect(err).NotTo(HaveOccurred())
	return signer
}

func tenantCtx(tenantID string) context.Context {
	ctx, err := tenant.WithTenant(context.Background(), tenantID)
	Expect(err).NotTo(HaveOccurred())
	return ctx
}

// callerCtx is what the gateway's auth interceptor sets: the caller's tenant
// and identity.
func callerCtx(tenantID string, roles ...string) context.Context {
	return authn.WithIdentity(tenantCtx(tenantID), &authn.Identity{Subject: reviewer, TenantID: tenantID, Roles: roles})
}

func tc(tenantID string) *pb.TenantContext { return &pb.TenantContext{TenantId: tenantID} }

// connectMessage returns err's Connect message; it fails the spec when err is
// not a Connect error.
func connectMessage(err error) string {
	var ce *connect.Error
	Expect(errors.As(err, &ce)).To(BeTrue(), "want a Connect error, got %v", err)
	return ce.Message()
}

// rpc calls one EdgeCleaningService RPC for tenantID and returns its error.
type rpc func(ctx context.Context, svc *cleaningsvc.Service, tenantID string) error

var (
	runPreview rpc = func(ctx context.Context, svc *cleaningsvc.Service, tenantID string) error {
		_, err := svc.RunCleaning(ctx, connect.NewRequest(&pb.RunCleaningRequest{
			TenantContext: tc(tenantID), Mode: pb.CleaningMode_CLEANING_MODE_PREVIEW,
		}))
		return err
	}
	listFindings rpc = func(ctx context.Context, svc *cleaningsvc.Service, tenantID string) error {
		_, err := svc.ListFindings(ctx, connect.NewRequest(&pb.ListFindingsRequest{TenantContext: tc(tenantID)}))
		return err
	}
	getUnknown rpc = func(ctx context.Context, svc *cleaningsvc.Service, tenantID string) error {
		_, err := svc.GetFinding(ctx, connect.NewRequest(&pb.GetFindingRequest{TenantContext: tc(tenantID), FindingId: "unknown"}))
		return err
	}
	decideUnknown rpc = func(ctx context.Context, svc *cleaningsvc.Service, tenantID string) error {
		_, err := svc.DecideFinding(ctx, connect.NewRequest(&pb.DecideFindingRequest{
			TenantContext: tc(tenantID), FindingId: "unknown", Verdict: cleaning.VerdictDismiss,
			Rationale: "not a problem", ExpectStateToken: "token",
		}))
		return err
	}
)

// state is everything a handler call may write.
type state struct {
	Findings  []cleaning.StoredFinding
	Decisions [][]cleaning.Decision
	Edges     []graphdb.Relationship
}

func snapshot(store *cleaningtest.Fake, g graphdb.GraphDB) state {
	ctx := tenantCtx(tenantA)
	fs, err := store.Findings(ctx, tenantA, cleaning.Scope{})
	Expect(err).NotTo(HaveOccurred())
	s := state{Findings: fs}
	for _, f := range fs {
		_, ds, err := store.GetFinding(ctx, tenantA, f.ID)
		Expect(err).NotTo(HaveOccurred())
		s.Decisions = append(s.Decisions, ds)
	}
	s.Edges, err = g.QueryRelationships(ctx, tenantA, graphdb.RelationshipQuery{EdgeLabel: results.SemanticMatchEdgeLabel})
	Expect(err).NotTo(HaveOccurred())
	return s
}

// failSupersede fails every SupersedeFact with a driver-style error, leaving
// an apply's retirements recorded but not projected.
type failSupersede struct {
	graphdb.GraphDB
	err error
}

func (f *failSupersede) SupersedeFact(context.Context, string, graphdb.SupersedeRequest) (bool, error) {
	return false, f.err
}

var _ = Describe("EdgeCleaningService", func() {
	var (
		g     graphdb.GraphDB
		store *cleaningtest.Fake
		svc   *cleaningsvc.Service
		admin context.Context
		rev   context.Context
	)

	newService := func(signer attestation.Generator) *cleaningsvc.Service {
		return cleaningsvc.NewService(cleaning.NewEngine(store, g, fixturePolicy(), nil, signer,
			cleaning.WithClock(func() time.Time { return fixtureNow })))
	}
	preview := func() *pb.RunCleaningResponse {
		resp, err := svc.RunCleaning(admin, connect.NewRequest(&pb.RunCleaningRequest{
			TenantContext: tc(tenantA), Mode: pb.CleaningMode_CLEANING_MODE_PREVIEW,
		}))
		Expect(err).NotTo(HaveOccurred())
		return resp.Msg
	}
	apply := func() *pb.RunCleaningResponse {
		resp, err := svc.RunCleaning(admin, connect.NewRequest(&pb.RunCleaningRequest{
			TenantContext: tc(tenantA), Mode: pb.CleaningMode_CLEANING_MODE_APPLY, ExpectToken: preview().GetPlanToken(),
		}))
		Expect(err).NotTo(HaveOccurred())
		return resp.Msg
	}
	get := func(ctx context.Context, id string) *pb.GetFindingResponse {
		resp, err := svc.GetFinding(ctx, connect.NewRequest(&pb.GetFindingRequest{TenantContext: tc(tenantA), FindingId: id}))
		Expect(err).NotTo(HaveOccurred())
		return resp.Msg
	}

	BeforeEach(func() {
		g = fixtureGraph()
		store = fixtureStore()
		svc = newService(newSigner())
		admin = callerCtx(tenantA, authn.RoleAdmin)
		rev = callerCtx(tenantA, authn.RoleReviewer)
	})

	Describe("authorization", func() {
		DescribeTable("a caller holding a role the RPC does not accept is refused with PermissionDenied naming the role",
			func(call rpc, roles []string, want string) {
				err := call(callerCtx(tenantA, roles...), svc, tenantA)
				Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
				Expect(connectMessage(err)).To(ContainSubstring(want))
			},
			Entry("RunCleaning, reviewer", runPreview, []string{authn.RoleReviewer}, "admin role"),
			Entry("RunCleaning, neither", runPreview, []string{"reader"}, "admin role"),
			Entry("ListFindings, neither", listFindings, []string{"reader"}, "reviewer or admin role"),
			Entry("GetFinding, neither", getUnknown, []string{"reader"}, "reviewer or admin role"),
			Entry("DecideFinding, admin", decideUnknown, []string{authn.RoleAdmin}, decideDenied),
			Entry("DecideFinding, neither", decideUnknown, []string{"reader"}, decideDenied),
		)

		DescribeTable("a caller holding a role the RPC accepts reaches the engine",
			func(call rpc, roles []string, want connect.Code) {
				err := call(callerCtx(tenantA, roles...), svc, tenantA)
				if want == 0 {
					Expect(err).NotTo(HaveOccurred())
					return
				}
				Expect(connect.CodeOf(err)).To(Equal(want))
			},
			Entry("RunCleaning, admin: the preview succeeds", runPreview, []string{authn.RoleAdmin}, connect.Code(0)),
			Entry("RunCleaning, both: the preview succeeds", runPreview, []string{authn.RoleAdmin, authn.RoleReviewer}, connect.Code(0)),
			Entry("ListFindings, admin: the list succeeds", listFindings, []string{authn.RoleAdmin}, connect.Code(0)),
			Entry("ListFindings, reviewer: the list succeeds", listFindings, []string{authn.RoleReviewer}, connect.Code(0)),
			Entry("ListFindings, both: the list succeeds", listFindings, []string{authn.RoleAdmin, authn.RoleReviewer}, connect.Code(0)),
			Entry("GetFinding, admin: the unknown finding is NotFound", getUnknown, []string{authn.RoleAdmin}, connect.CodeNotFound),
			Entry("GetFinding, reviewer: the unknown finding is NotFound", getUnknown, []string{authn.RoleReviewer}, connect.CodeNotFound),
			Entry("GetFinding, both: the unknown finding is NotFound", getUnknown, []string{authn.RoleAdmin, authn.RoleReviewer}, connect.CodeNotFound),
			Entry("DecideFinding, reviewer: the unknown finding is NotFound", decideUnknown, []string{authn.RoleReviewer}, connect.CodeNotFound),
			Entry("DecideFinding, both: the unknown finding is NotFound", decideUnknown, []string{authn.RoleAdmin, authn.RoleReviewer}, connect.CodeNotFound),
		)

		DescribeTable("a caller without an identity is refused with Unauthenticated",
			func(call rpc) {
				Expect(connect.CodeOf(call(tenantCtx(tenantA), svc, tenantA))).To(Equal(connect.CodeUnauthenticated))
			},
			Entry("RunCleaning", runPreview),
			Entry("ListFindings", listFindings),
			Entry("GetFinding", getUnknown),
			Entry("DecideFinding", decideUnknown),
		)
	})

	Describe("tenant scoping", func() {
		all := []rpc{runPreview, listFindings, getUnknown, decideUnknown}
		both := []string{authn.RoleAdmin, authn.RoleReviewer}

		It("refuses a tenant_context other than the caller's with PermissionDenied naming neither tenant, and writes nothing", func() {
			apply()
			before := snapshot(store, g)
			for _, call := range all {
				err := call(callerCtx(tenantA, both...), svc, tenantB)
				Expect(connect.CodeOf(err)).To(Equal(connect.CodePermissionDenied))
				msg := connectMessage(err)
				Expect(msg).To(ContainSubstring("Send the request for your own tenant"))
				Expect(msg).NotTo(Or(ContainSubstring(tenantA), ContainSubstring(tenantB)))
			}
			Expect(snapshot(store, g)).To(Equal(before))
		})

		It("refuses a request without tenant_context with InvalidArgument", func() {
			for _, call := range all {
				err := call(callerCtx(tenantA, both...), svc, "")
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
				Expect(connectMessage(err)).To(ContainSubstring("tenant_context is required"))
			}
		})

		It("reports another tenant's finding as NotFound without naming either tenant", func() {
			apply()
			other := callerCtx(tenantB, both...)
			before := snapshot(store, g)

			_, err := svc.GetFinding(other, connect.NewRequest(&pb.GetFindingRequest{TenantContext: tc(tenantB), FindingId: fanout}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
			Expect(connectMessage(err)).NotTo(Or(ContainSubstring(tenantA), ContainSubstring(tenantB)))

			_, err = svc.DecideFinding(other, connect.NewRequest(&pb.DecideFindingRequest{
				TenantContext: tc(tenantB), FindingId: fanout, Verdict: cleaning.VerdictDismiss,
				Rationale: "x", ExpectStateToken: get(rev, fanout).GetStateToken(),
			}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeNotFound))
			Expect(connectMessage(err)).NotTo(Or(ContainSubstring(tenantA), ContainSubstring(tenantB)))
			Expect(snapshot(store, g)).To(Equal(before))
		})
	})

	Describe("RunCleaning", func() {
		It("previews findings readable without decoding IDs, and writes nothing", func() {
			before := snapshot(store, g)
			resp := preview()

			Expect(resp.GetPlanToken()).NotTo(BeEmpty())
			Expect(resp.GetRetireVerdicts()).To(Equal(int32(1)))
			Expect(resp.GetProjected()).To(BeZero())
			Expect(resp.GetSummary().GetVerdicts()).To(Equal(int32(3)))
			Expect(resp.GetSummary().GetPairs()).To(BeNumerically(">", 0))
			byID := map[string]*pb.PlannedFinding{}
			for _, p := range resp.GetFindings() {
				byID[p.GetFinding().GetFindingId()] = p
			}
			Expect(byID).To(HaveKey(conflict))
			c := byID[conflict]
			Expect(c.GetAction()).To(Equal(string(cleaning.ActionRetire)))
			Expect(c.GetChange()).To(Equal(string(cleaning.ChangeNew)))
			Expect(c.GetToStatus()).To(Equal(string(cleaning.StatusRetired)))
			Expect(c.GetFinding().GetSignal()).To(Equal(string(cleaning.SignalConflictsWith)))
			Expect(c.GetFinding().GetTargets()).To(HaveExactElements(
				And(HaveField("JobId", "j1"), HaveField("SourceNodeId", "a2"), HaveField("TargetNodeId", "b1"))))
			Expect(c.GetFinding().GetVerdicts()).To(HaveLen(1))
			v := c.GetFinding().GetVerdicts()[0]
			Expect(v.GetSource()).To(And(HaveField("Identifier", "AC-3"), HaveField("Label", "AC-3"),
				HaveField("Title", "Access Enforcement"), HaveField("Catalog", "NIST SP 800-53 5.1.1")))
			Expect(v.GetTarget()).To(And(HaveField("Identifier", "03.01.01"), HaveField("Label", "03.01.01"),
				HaveField("Title", "Account Management"), HaveField("Catalog", "NIST SP 800-171 r3")))
			Expect(v.GetRelationship()).NotTo(BeEmpty())
			Expect(v.GetJobCompletedAt().AsTime()).To(Equal(fixtureTime))
			Expect(v.GetEdgeIds()).To(ConsistOf("e-a2-b1"))
			Expect(snapshot(store, g)).To(Equal(before))
		})

		It("shows a job-level finding's job readable without decoding IDs, and no job on other findings", func() {
			stale := job
			stale.Stages = map[string]*cleaning.StageRecord{"relationship": {PromptName: "relationship", PromptVersion: "1"}}
			store.Jobs[tenantA] = []cleaning.Job{stale}
			current := func(context.Context, string, string) (cleaning.StageRecord, bool, error) {
				return cleaning.StageRecord{PromptName: "relationship", PromptVersion: "2"}, true, nil
			}
			svc = cleaningsvc.NewService(cleaning.NewEngine(store, g, fixturePolicy(), current, newSigner(),
				cleaning.WithClock(func() time.Time { return fixtureNow })))

			var found *pb.Finding
			for _, pf := range preview().GetFindings() {
				if pf.GetFinding().GetSignal() != string(cleaning.SignalStalePrompt) {
					Expect(pf.GetFinding().GetJob()).To(BeNil())
					continue
				}
				found = pf.GetFinding()
			}
			Expect(found).NotTo(BeNil())
			Expect(found.GetVerdicts()).To(BeEmpty())
			Expect(found.GetTargets()).To(BeEmpty(), "report keeps no targets")
			Expect(found.GetJob().GetJobId()).To(Equal("j1"))
			Expect(found.GetJob().GetCompletedAt().AsTime()).To(Equal(fixtureTime))
			Expect(found.GetJob().GetSourceCatalog()).To(Equal("NIST SP 800-53 5.1.1"))
			Expect(found.GetJob().GetTargetCatalog()).To(Equal("NIST SP 800-171 r3"))
			Expect(found.GetJob().GetVerdictsInScope()).To(Equal(int64(3)))
			Expect(found.GetDetail()).To(HaveKeyWithValue("relationship.current", "2"))
		})

		It("applies a previewed plan and returns its token", func() {
			token := preview().GetPlanToken()
			resp, err := svc.RunCleaning(admin, connect.NewRequest(&pb.RunCleaningRequest{
				TenantContext: tc(tenantA), Mode: pb.CleaningMode_CLEANING_MODE_APPLY, ExpectToken: token,
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetPlanToken()).To(Equal(token))
			Expect(resp.Msg.GetRetireVerdicts()).To(Equal(int32(1)))
			Expect(get(rev, conflict).GetFinding().GetStatus()).To(Equal(string(cleaning.StatusRetired)))
		})

		It("reports a part-way apply with FailedPrecondition, the count and the resume instruction, then resumes it", func() {
			token := preview().GetPlanToken()
			real := g
			g = &failSupersede{GraphDB: real, err: errors.New("graph driver: connection reset by 10.0.0.9")}
			svc = newService(newSigner())
			_, err := svc.RunCleaning(admin, connect.NewRequest(&pb.RunCleaningRequest{
				TenantContext: tc(tenantA), Mode: pb.CleaningMode_CLEANING_MODE_APPLY, ExpectToken: token,
			}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			msg := connectMessage(err)
			Expect(msg).To(ContainSubstring("0 of 1 findings applied to the graph"))
			Expect(msg).To(ContainSubstring("run 'crosscodex admin cleaning run --resume' to finish"))
			Expect(msg).NotTo(ContainSubstring("graph driver"))
			Expect(msg).NotTo(ContainSubstring("10.0.0.9"))

			g = real
			svc = newService(newSigner())
			resp, err := svc.RunCleaning(admin, connect.NewRequest(&pb.RunCleaningRequest{
				TenantContext: tc(tenantA), Mode: pb.CleaningMode_CLEANING_MODE_RESUME,
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetProjected()).To(Equal(int32(1)))
			Expect(resp.Msg.GetPlanToken()).To(BeEmpty())
		})

		It("refuses an apply whose plan changed with FailedPrecondition and the engine's instruction, and writes nothing", func() {
			before := snapshot(store, g)
			_, err := svc.RunCleaning(admin, connect.NewRequest(&pb.RunCleaningRequest{
				TenantContext: tc(tenantA), Mode: pb.CleaningMode_CLEANING_MODE_APPLY, ExpectToken: "stale",
			}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			Expect(connectMessage(err)).To(ContainSubstring("Preview again and apply the new token"))
			Expect(snapshot(store, g)).To(Equal(before))
		})

		DescribeTable("refuses a malformed request with InvalidArgument and an instruction, and writes nothing",
			func(req *pb.RunCleaningRequest, want string) {
				before := snapshot(store, g)
				req.TenantContext = tc(tenantA)
				_, err := svc.RunCleaning(admin, connect.NewRequest(req))
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
				Expect(connectMessage(err)).To(ContainSubstring(want))
				Expect(snapshot(store, g)).To(Equal(before))
			},
			Entry("no mode", &pb.RunCleaningRequest{}, "CLEANING_MODE_PREVIEW"),
			Entry("an unknown mode", &pb.RunCleaningRequest{Mode: pb.CleaningMode(42)}, "CLEANING_MODE_PREVIEW"),
			Entry("apply without expect_token", &pb.RunCleaningRequest{Mode: pb.CleaningMode_CLEANING_MODE_APPLY},
				"expect_token is required"),
			Entry("a scope with only the source catalog", &pb.RunCleaningRequest{
				Mode: pb.CleaningMode_CLEANING_MODE_PREVIEW, Scope: &pb.CatalogPairScope{SourceCatalogId: "cat-a"},
			}, "set both"),
			Entry("a scope with only the target catalog", &pb.RunCleaningRequest{
				Mode: pb.CleaningMode_CLEANING_MODE_APPLY, ExpectToken: "t", Scope: &pb.CatalogPairScope{TargetCatalogId: "cat-b"},
			}, "set both"),
		)

		DescribeTable("refuses a resume with a scope with InvalidArgument, and projects nothing",
			func(scope *pb.CatalogPairScope) {
				token := preview().GetPlanToken()
				real := g
				g = &failSupersede{GraphDB: real, err: errors.New("graph driver: connection reset")}
				svc = newService(newSigner())
				_, err := svc.RunCleaning(admin, connect.NewRequest(&pb.RunCleaningRequest{
					TenantContext: tc(tenantA), Mode: pb.CleaningMode_CLEANING_MODE_APPLY, ExpectToken: token,
				}))
				Expect(err).To(HaveOccurred())
				g = real
				svc = newService(newSigner())
				before := snapshot(store, g)
				Expect(before.Findings).To(ContainElement(HaveField("GraphApplied", BeFalse())), "a resume has work to do")

				_, err = svc.RunCleaning(admin, connect.NewRequest(&pb.RunCleaningRequest{
					TenantContext: tc(tenantA), Mode: pb.CleaningMode_CLEANING_MODE_RESUME, Scope: scope,
				}))
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
				Expect(connectMessage(err)).To(ContainSubstring("CLEANING_MODE_RESUME projects every recorded change and takes no scope: omit scope"))
				Expect(snapshot(store, g)).To(Equal(before))
			},
			Entry("both catalogs", &pb.CatalogPairScope{SourceCatalogId: "cat-a", TargetCatalogId: "cat-b"}),
			Entry("only the source catalog", &pb.CatalogPairScope{SourceCatalogId: "cat-a"}),
			Entry("only the target catalog", &pb.CatalogPairScope{TargetCatalogId: "cat-b"}),
		)

		It("refuses an apply without a signer with FailedPrecondition naming the settings, and writes nothing", func() {
			token := preview().GetPlanToken()
			svc = newService(nil)
			before := snapshot(store, g)
			_, err := svc.RunCleaning(admin, connect.NewRequest(&pb.RunCleaningRequest{
				TenantContext: tc(tenantA), Mode: pb.CleaningMode_CLEANING_MODE_APPLY, ExpectToken: token,
			}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			Expect(connectMessage(err)).To(ContainSubstring("attestation.private_key_path"))
			Expect(snapshot(store, g)).To(Equal(before))
		})

		It("sets the audit correlation ID to the active span's trace ID", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(tp.Shutdown, context.Background())
			ctx, span := tp.TracerProvider().Tracer("test").Start(admin, "request")
			defer span.End()
			resp, err := svc.RunCleaning(ctx, connect.NewRequest(&pb.RunCleaningRequest{
				TenantContext: tc(tenantA), Mode: pb.CleaningMode_CLEANING_MODE_PREVIEW,
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetAudit().GetCorrelationId()).To(Equal(span.SpanContext().TraceID().String()))
		})
	})

	Describe("ListFindings", func() {
		BeforeEach(func() { apply() })

		It("pages the tenant's findings and filters them by status", func() {
			resp, err := svc.ListFindings(rev, connect.NewRequest(&pb.ListFindingsRequest{
				TenantContext: tc(tenantA), Pagination: &pb.Pagination{PageSize: 1},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetFindings()).To(HaveLen(1))
			next := resp.Msg.GetPageInfo().GetNextPageToken()
			Expect(next).NotTo(BeEmpty())

			resp, err = svc.ListFindings(rev, connect.NewRequest(&pb.ListFindingsRequest{
				TenantContext: tc(tenantA), Pagination: &pb.Pagination{PageSize: 1, PageToken: next},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetFindings()).To(HaveLen(1))

			resp, err = svc.ListFindings(rev, connect.NewRequest(&pb.ListFindingsRequest{
				TenantContext: tc(tenantA), Status: string(cleaning.StatusPending),
				Scope: &pb.CatalogPairScope{SourceCatalogId: "cat-b", TargetCatalogId: "cat-a"},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetFindings()).To(HaveExactElements(HaveField("FindingId", fanout)))
			Expect(resp.Msg.GetFindings()[0].GetVerdicts()).To(HaveEach(HaveField("Source.Title", "Account Management")))
			Expect(resp.Msg.GetPageInfo().GetNextPageToken()).To(BeEmpty())
		})

		It("returns both findings on one page when page_size is 0 (the default page)", func() {
			resp, err := svc.ListFindings(rev, connect.NewRequest(&pb.ListFindingsRequest{TenantContext: tc(tenantA)}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetFindings()).To(HaveLen(2))
			Expect(resp.Msg.GetPageInfo().GetNextPageToken()).To(BeEmpty())
		})

		DescribeTable("refuses a malformed listing with InvalidArgument and an instruction",
			func(req *pb.ListFindingsRequest, want string) {
				req.TenantContext = tc(tenantA)
				_, err := svc.ListFindings(rev, connect.NewRequest(req))
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
				Expect(connectMessage(err)).To(ContainSubstring(want))
			},
			Entry("a negative page_size", &pb.ListFindingsRequest{Pagination: &pb.Pagination{PageSize: -1}},
				"use a value from 0 to 1000"),
			Entry("a page_size over 1000", &pb.ListFindingsRequest{Pagination: &pb.Pagination{PageSize: 1001}},
				"use a value from 0 to 1000"),
			Entry("a scope with one catalog", &pb.ListFindingsRequest{Scope: &pb.CatalogPairScope{SourceCatalogId: "cat-a"}},
				"set both"),
			Entry("an unknown signal, listing the accepted signals", &pb.ListFindingsRequest{Signal: "low_agreemnt"},
				`signal "low_agreemnt" is not a cleaning signal: use one of equivalence_fanout, cross_job_contradiction, `+
					`low_agreement, conflicts_with, stale_prompt, stale_model, or leave it empty for all`),
			Entry("an unknown status, listing the accepted statuses", &pb.ListFindingsRequest{Status: "retierd"},
				`status "retierd" is not a finding status: use one of reported, pending, retired, dismissed, restored, cleared, `+
					`stale, or leave it empty for all`),
		)

		It("filters by signal", func() {
			resp, err := svc.ListFindings(rev, connect.NewRequest(&pb.ListFindingsRequest{
				TenantContext: tc(tenantA), Signal: string(cleaning.SignalConflictsWith),
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetFindings()).To(HaveExactElements(HaveField("FindingId", conflict)))
		})

		It("filters by status stale", func() {
			ctx := tenantCtx(tenantA)
			f, _, err := store.GetFinding(ctx, tenantA, conflict)
			Expect(err).NotTo(HaveOccurred())
			Expect(store.MarkStale(ctx, tenantA, cleaning.Decision{
				ID: "d-stale", FindingID: conflict, Verdict: cleaning.VerdictStale, DecidedBy: cleaning.DecidedByReproject,
				Rationale: "the graph no longer holds verdicts", Request: []byte("{}"), Attestation: []byte("sig"),
			}, cleaning.StateToken(f))).To(Succeed())

			resp, err := svc.ListFindings(rev, connect.NewRequest(&pb.ListFindingsRequest{
				TenantContext: tc(tenantA), Status: string(cleaning.StatusStale),
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetFindings()).To(HaveExactElements(And(
				HaveField("FindingId", conflict), HaveField("Status", string(cleaning.StatusStale)))))
		})
	})

	Describe("GetFinding and DecideFinding", func() {
		BeforeEach(func() { apply() })

		It("records the authenticated subject as the decider and returns the finding afterwards", func() {
			read := get(rev, fanout)
			Expect(read.GetFinding().GetStatus()).To(Equal(string(cleaning.StatusPending)))

			resp, err := svc.DecideFinding(rev, connect.NewRequest(&pb.DecideFindingRequest{
				TenantContext: tc(tenantA), FindingId: fanout, Verdict: cleaning.VerdictDismiss,
				Rationale: "both mappings hold", ExpectStateToken: read.GetStateToken(),
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetFinding().GetStatus()).To(Equal(string(cleaning.StatusDismissed)))
			Expect(resp.Msg.GetDecisionId()).NotTo(BeEmpty())
			Expect(resp.Msg.GetStateToken()).NotTo(Equal(read.GetStateToken()))

			_, ds, err := store.GetFinding(tenantCtx(tenantA), tenantA, fanout)
			Expect(err).NotTo(HaveOccurred())
			Expect(ds).To(HaveExactElements(And(HaveField("ID", resp.Msg.GetDecisionId()), HaveField("DecidedBy", reviewer),
				HaveField("Verdict", cleaning.VerdictDismiss), HaveField("Rationale", "both mappings hold"))))
		})

		It("returns the decision history with the attestation digest and the state token", func() {
			ctx := tenantCtx(tenantA)
			_, ds, err := store.GetFinding(ctx, tenantA, conflict)
			Expect(err).NotTo(HaveOccurred())
			Expect(ds).To(HaveLen(1))
			sum := sha256.Sum256(ds[0].Attestation)

			resp := get(rev, conflict)
			Expect(resp.GetDecisions()).To(HaveExactElements(And(
				HaveField("DecisionId", ds[0].ID), HaveField("Verdict", cleaning.VerdictRetire),
				HaveField("DecidedBy", cleaning.DecidedByPolicy), HaveField("RunId", ds[0].RunID),
				HaveField("AttestationSha256", hex.EncodeToString(sum[:])))))
			Expect(resp.GetDecisions()[0].GetDecidedAt().AsTime()).To(Equal(ds[0].DecidedAt))
			f, _, err := store.GetFinding(ctx, tenantA, conflict)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.GetStateToken()).To(Equal(cleaning.StateToken(f)))
			Expect(resp.GetFinding().GetEvidenceHash()).To(Equal(f.EvidenceHash))
			Expect(resp.GetFinding().GetUpdatedAt().AsTime()).To(Equal(f.UpdatedAt))
		})

		It("retires the verdicts a decision names", func() {
			resp, err := svc.DecideFinding(rev, connect.NewRequest(&pb.DecideFindingRequest{
				TenantContext: tc(tenantA), FindingId: fanout, Verdict: cleaning.VerdictRetire, Rationale: "b2 is the wrong match",
				ExpectStateToken: get(rev, fanout).GetStateToken(),
				RetireVerdicts:   []*pb.VerdictRef{{JobId: "j1", SourceNodeId: "a1", TargetNodeId: "b2"}},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetFinding().GetStatus()).To(Equal(string(cleaning.StatusRetired)))
			Expect(resp.Msg.GetFinding().GetTargets()).To(HaveExactElements(
				And(HaveField("JobId", "j1"), HaveField("SourceNodeId", "a1"), HaveField("TargetNodeId", "b2"))))
		})

		It("reports a decision whose graph update failed with FailedPrecondition, saying it was recorded, the count and to resume", func() {
			token := get(rev, fanout).GetStateToken()
			g = &failSupersede{GraphDB: g, err: errors.New("graph driver: connection reset by 10.0.0.9")}
			svc = newService(newSigner())
			_, err := svc.DecideFinding(rev, connect.NewRequest(&pb.DecideFindingRequest{
				TenantContext: tc(tenantA), FindingId: fanout, Verdict: cleaning.VerdictRetire, Rationale: "b2 is the wrong match",
				ExpectStateToken: token, RetireVerdicts: []*pb.VerdictRef{{JobId: "j1", SourceNodeId: "a1", TargetNodeId: "b2"}},
			}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			_, ds, gerr := store.GetFinding(tenantCtx(tenantA), tenantA, fanout)
			Expect(gerr).NotTo(HaveOccurred())
			Expect(ds).To(HaveExactElements(HaveField("Verdict", cleaning.VerdictRetire)))
			msg := connectMessage(err)
			Expect(msg).To(ContainSubstring("decision " + ds[0].ID + " was recorded and stands"))
			Expect(msg).To(ContainSubstring("0 of 1 findings applied to the graph"))
			Expect(msg).To(ContainSubstring("run 'crosscodex admin cleaning run --resume' to finish"))
			Expect(msg).NotTo(ContainSubstring("graph driver"))
			Expect(msg).NotTo(ContainSubstring("10.0.0.9"))
		})

		It("sets the audit correlation ID of GetFinding and DecideFinding to the active span's trace ID", func() {
			tp, err := telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(tp.Shutdown, context.Background())
			ctx, span := tp.TracerProvider().Tracer("test").Start(rev, "request")
			defer span.End()
			trace := span.SpanContext().TraceID().String()

			read, err := svc.GetFinding(ctx, connect.NewRequest(&pb.GetFindingRequest{TenantContext: tc(tenantA), FindingId: fanout}))
			Expect(err).NotTo(HaveOccurred())
			Expect(read.Msg.GetAudit().GetCorrelationId()).To(Equal(trace))
			resp, err := svc.DecideFinding(ctx, connect.NewRequest(&pb.DecideFindingRequest{
				TenantContext: tc(tenantA), FindingId: fanout, Verdict: cleaning.VerdictDismiss,
				Rationale: "x", ExpectStateToken: read.Msg.GetStateToken(),
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Msg.GetAudit().GetCorrelationId()).To(Equal(trace))
		})

		DescribeTable("refuses a decision with the mapped code and the engine's instruction, and writes nothing",
			func(edit func(*pb.DecideFindingRequest), code connect.Code, want string) {
				req := &pb.DecideFindingRequest{
					TenantContext: tc(tenantA), FindingId: fanout, Verdict: cleaning.VerdictDismiss,
					Rationale: "x", ExpectStateToken: get(rev, fanout).GetStateToken(),
				}
				edit(req)
				before := snapshot(store, g)
				_, err := svc.DecideFinding(rev, connect.NewRequest(req))
				Expect(connect.CodeOf(err)).To(Equal(code))
				Expect(connectMessage(err)).To(ContainSubstring(want))
				Expect(snapshot(store, g)).To(Equal(before))
			},
			Entry("no finding_id: InvalidArgument", func(r *pb.DecideFindingRequest) { r.FindingId = "" },
				connect.CodeInvalidArgument, "finding_id is required"),
			Entry("no expect_state_token: InvalidArgument", func(r *pb.DecideFindingRequest) { r.ExpectStateToken = "" },
				connect.CodeInvalidArgument, "expect_state_token is required"),
			Entry("a retire verdict with ':' in a part: InvalidArgument", func(r *pb.DecideFindingRequest) {
				r.Verdict = cleaning.VerdictRetire
				r.RetireVerdicts = []*pb.VerdictRef{{JobId: "j1", SourceNodeId: "a1:x", TargetNodeId: "b2"}}
			}, connect.CodeInvalidArgument, "retire_verdicts[0]"),
			Entry("a retire verdict with an empty part: InvalidArgument", func(r *pb.DecideFindingRequest) {
				r.Verdict = cleaning.VerdictRetire
				r.RetireVerdicts = []*pb.VerdictRef{{JobId: "j1", SourceNodeId: "a1", TargetNodeId: "b1"}, {JobId: "j1", SourceNodeId: "a1"}}
			}, connect.CodeInvalidArgument, "retire_verdicts[1]"),
			Entry("a retire verdict outside the finding: InvalidArgument", func(r *pb.DecideFindingRequest) {
				r.Verdict = cleaning.VerdictRetire
				r.RetireVerdicts = []*pb.VerdictRef{{JobId: "j1", SourceNodeId: "a2", TargetNodeId: "b1"}}
			}, connect.CodeInvalidArgument, "name only verdicts the finding concerns"),
			Entry("a retire naming no verdicts of a finding without targets: InvalidArgument", func(r *pb.DecideFindingRequest) {
				r.Verdict = cleaning.VerdictRetire
			}, connect.CodeInvalidArgument, "--retire-verdict"),
			Entry("an unknown verdict: InvalidArgument", func(r *pb.DecideFindingRequest) { r.Verdict = "delete" },
				connect.CodeInvalidArgument, "verdict \"delete\""),
			Entry("an empty rationale: InvalidArgument", func(r *pb.DecideFindingRequest) { r.Rationale = "" },
				connect.CodeInvalidArgument, "Pass --rationale"),
			Entry("a whitespace-only rationale: InvalidArgument", func(r *pb.DecideFindingRequest) { r.Rationale = " \t\n" },
				connect.CodeInvalidArgument, "Pass --rationale"),
			Entry("a stale state token: FailedPrecondition", func(r *pb.DecideFindingRequest) { r.ExpectStateToken = "stale" },
				connect.CodeFailedPrecondition, "decide again"),
			Entry("a transition the status does not allow: FailedPrecondition", func(r *pb.DecideFindingRequest) {
				r.Verdict = cleaning.VerdictRestore
			}, connect.CodeFailedPrecondition, "allowed now"),
		)

		It("refuses an unregistered decider with FailedPrecondition, and writes nothing", func() {
			store.Reviewers[tenantA] = map[string]string{}
			before := snapshot(store, g)
			_, err := svc.DecideFinding(rev, connect.NewRequest(&pb.DecideFindingRequest{
				TenantContext: tc(tenantA), FindingId: fanout, Verdict: cleaning.VerdictDismiss,
				Rationale: "x", ExpectStateToken: get(rev, fanout).GetStateToken(),
			}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition))
			Expect(connectMessage(err)).To(ContainSubstring(reviewer))
			Expect(snapshot(store, g)).To(Equal(before))
		})

		It("refuses a get without finding_id with InvalidArgument", func() {
			_, err := svc.GetFinding(rev, connect.NewRequest(&pb.GetFindingRequest{TenantContext: tc(tenantA)}))
			Expect(connect.CodeOf(err)).To(Equal(connect.CodeInvalidArgument))
			Expect(connectMessage(err)).To(ContainSubstring("finding_id is required"))
		})
	})

	Describe("engine error mapping", func() {
		var stub *stubEngine

		BeforeEach(func() {
			stub = &stubEngine{}
			svc = cleaningsvc.NewService(stub)
		})

		calls := map[string]rpc{
			"RunCleaning preview": runPreview,
			"RunCleaning apply": func(ctx context.Context, svc *cleaningsvc.Service, tenantID string) error {
				_, err := svc.RunCleaning(ctx, connect.NewRequest(&pb.RunCleaningRequest{
					TenantContext: tc(tenantID), Mode: pb.CleaningMode_CLEANING_MODE_APPLY, ExpectToken: "t",
				}))
				return err
			},
			"RunCleaning resume": func(ctx context.Context, svc *cleaningsvc.Service, tenantID string) error {
				_, err := svc.RunCleaning(ctx, connect.NewRequest(&pb.RunCleaningRequest{
					TenantContext: tc(tenantID), Mode: pb.CleaningMode_CLEANING_MODE_RESUME,
				}))
				return err
			},
			"ListFindings":  listFindings,
			"GetFinding":    getUnknown,
			"DecideFinding": decideUnknown,
		}
		both := []string{authn.RoleAdmin, authn.RoleReviewer}

		DescribeTable("maps each engine sentinel to its code and passes the engine's message through on every RPC",
			func(sentinel error, code connect.Code) {
				stub.err = fmt.Errorf("cleaning op: finding f1: %w: what to do next", sentinel)
				for name, call := range calls {
					err := call(callerCtx(tenantA, both...), svc, tenantA)
					Expect(connect.CodeOf(err)).To(Equal(code), name)
					Expect(connectMessage(err)).To(Equal(stub.err.Error()), name)
				}
			},
			Entry("ErrProjectionIncomplete: FailedPrecondition", cleaning.ErrProjectionIncomplete, connect.CodeFailedPrecondition),
			Entry("ErrTenantMismatch: PermissionDenied", cleaning.ErrTenantMismatch, connect.CodePermissionDenied),
			Entry("ErrFindingNotFound: NotFound", cleaning.ErrFindingNotFound, connect.CodeNotFound),
			Entry("ErrInvalidScope: InvalidArgument", cleaning.ErrInvalidScope, connect.CodeInvalidArgument),
			Entry("ErrInvalidDecision: InvalidArgument", cleaning.ErrInvalidDecision, connect.CodeInvalidArgument),
			Entry("ErrVerdictNotInFinding: InvalidArgument", cleaning.ErrVerdictNotInFinding, connect.CodeInvalidArgument),
			Entry("ErrRetireNeedsVerdicts: InvalidArgument", cleaning.ErrRetireNeedsVerdicts, connect.CodeInvalidArgument),
			Entry("ErrTokenMismatch: FailedPrecondition", cleaning.ErrTokenMismatch, connect.CodeFailedPrecondition),
			Entry("ErrStateChanged: FailedPrecondition", cleaning.ErrStateChanged, connect.CodeFailedPrecondition),
			Entry("ErrUnprojected: FailedPrecondition", cleaning.ErrUnprojected, connect.CodeFailedPrecondition),
			Entry("ErrSigningDisabled: FailedPrecondition", cleaning.ErrSigningDisabled, connect.CodeFailedPrecondition),
			Entry("ErrInvalidTransition: FailedPrecondition", cleaning.ErrInvalidTransition, connect.CodeFailedPrecondition),
			Entry("ErrEdgeNotLive: FailedPrecondition", cleaning.ErrEdgeNotLive, connect.CodeFailedPrecondition),
			Entry("ErrRetireCapExceeded: FailedPrecondition", cleaning.ErrRetireCapExceeded, connect.CodeFailedPrecondition),
			Entry("ErrFindingTooLarge: FailedPrecondition", cleaning.ErrFindingTooLarge, connect.CodeFailedPrecondition),
			Entry("ErrReviewerNotRegistered: FailedPrecondition", cleaning.ErrReviewerNotRegistered, connect.CodeFailedPrecondition),
			Entry("ErrReviewerDeleted: FailedPrecondition", cleaning.ErrReviewerDeleted, connect.CodeFailedPrecondition),
		)

		It("replaces the message of a sentinel that also wraps a driver error with the sentinel's own text", func() {
			driver := errors.New("pq: could not serialize access at 10.0.0.9:5432 (role crosscodex_app)")
			stub.err = fmt.Errorf("apply cleaning run: %w: %w", cleaning.ErrTokenMismatch, driver)
			for name, call := range calls {
				err := call(callerCtx(tenantA, both...), svc, tenantA)
				Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition), name)
				msg := connectMessage(err)
				Expect(msg).To(Equal(cleaning.ErrTokenMismatch.Error()), name)
				Expect(msg).NotTo(ContainSubstring("pq:"), name)
			}
		})

		DescribeTable("sends a projection error's public text, without its driver cause, with FailedPrecondition",
			func(pe *cleaning.ProjectionError, want string) {
				stub.err = pe
				for name, call := range calls {
					err := call(callerCtx(tenantA, both...), svc, tenantA)
					Expect(connect.CodeOf(err)).To(Equal(connect.CodeFailedPrecondition), name)
					msg := connectMessage(err)
					Expect(msg).To(Equal(want), name)
					Expect(msg).NotTo(ContainSubstring("pq:"), name)
				}
			},
			Entry("an apply or resume",
				&cleaning.ProjectionError{Applied: 2, Total: 5, Err: errors.New("pq: connection reset by 10.0.0.9:5432")},
				"the graph update stopped part-way: 2 of 5 findings applied to the graph; run 'crosscodex admin cleaning run --resume' to finish"),
			Entry("a decision",
				&cleaning.ProjectionError{Applied: 0, Total: 1, DecisionID: "d1", Err: errors.New("pq: connection reset by 10.0.0.9:5432")},
				"decision d1 was recorded and stands, but the graph update stopped part-way: 0 of 1 findings applied to the graph; "+
					"run 'crosscodex admin cleaning run --resume' to finish"),
		)

		DescribeTable("returns any other engine error raw, for the gateway's error policy to withhold",
			func(engineErr error) {
				stub.err = engineErr
				for name, call := range calls {
					err := call(callerCtx(tenantA, both...), svc, tenantA)
					Expect(err).To(MatchError(engineErr), name)
					var ce *connect.Error
					Expect(errors.As(err, &ce)).To(BeFalse(), name)
				}
			},
			Entry("a driver error", errors.New("pq: relation edge_findings does not exist")),
			Entry("ErrEdgeLive", fmt.Errorf("restore: %w", cleaning.ErrEdgeLive)),
			Entry("ErrInvalidEvidence", fmt.Errorf("read finding: %w", cleaning.ErrInvalidEvidence)),
			Entry("ErrInvalidStageRecord", fmt.Errorf("job j1: %w", cleaning.ErrInvalidStageRecord)),
			Entry("ErrInvalidVerdictKey from stored data", fmt.Errorf("decode targets: %w", cleaning.ErrInvalidVerdictKey)),
		)

		It("passes the resolved tenant and the authenticated subject to the engine", func() {
			_, err := svc.DecideFinding(callerCtx(tenantA, authn.RoleReviewer), connect.NewRequest(&pb.DecideFindingRequest{
				TenantContext: tc(tenantA), FindingId: "f1", Verdict: cleaning.VerdictRetire, Rationale: "r",
				ExpectStateToken: "s", RetireVerdicts: []*pb.VerdictRef{{JobId: "j", SourceNodeId: "s", TargetNodeId: "t"}},
			}))
			Expect(err).NotTo(HaveOccurred())
			Expect(stub.tenants).To(ConsistOf(tenantA))
			Expect(stub.decided).To(ConsistOf(cleaning.DecideRequest{
				FindingID: "f1", Verdict: cleaning.VerdictRetire, Rationale: "r", ExpectState: "s", Reviewer: reviewer,
				RetireVerdicts: []cleaning.VerdictKey{{JobID: "j", SourceID: "s", TargetID: "t"}},
			}))
		})
	})
})

// stubEngine returns err from every method and records the tenants and
// decisions it was given. It is used only where the engine's result must be
// forced; every other spec runs the real engine.
type stubEngine struct {
	err     error
	tenants []string
	decided []cleaning.DecideRequest
}

func (s *stubEngine) Preview(_ context.Context, tenantID string, _ cleaning.Scope) (cleaning.Plan, error) {
	s.tenants = append(s.tenants, tenantID)
	return cleaning.Plan{}, s.err
}

func (s *stubEngine) Apply(_ context.Context, tenantID string, _ cleaning.Scope, _ string) (cleaning.Plan, error) {
	s.tenants = append(s.tenants, tenantID)
	return cleaning.Plan{}, s.err
}

func (s *stubEngine) Resume(_ context.Context, tenantID string) (int, error) {
	s.tenants = append(s.tenants, tenantID)
	return 0, s.err
}

func (s *stubEngine) Decide(_ context.Context, tenantID string, r cleaning.DecideRequest) (cleaning.StoredFinding, string, error) {
	s.tenants = append(s.tenants, tenantID)
	s.decided = append(s.decided, r)
	return cleaning.StoredFinding{}, "", s.err
}

func (s *stubEngine) ListFindings(_ context.Context, tenantID string, _ cleaning.FindingFilter) ([]cleaning.StoredFinding, string, error) {
	s.tenants = append(s.tenants, tenantID)
	return nil, "", s.err
}

func (s *stubEngine) GetFinding(_ context.Context, tenantID, _ string) (cleaning.StoredFinding, []cleaning.Decision, string, error) {
	s.tenants = append(s.tenants, tenantID)
	return cleaning.StoredFinding{}, nil, "", s.err
}
