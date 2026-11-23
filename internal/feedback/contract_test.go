package feedback_test

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"slices"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/internal/feedback"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// storeEnv is one spec's store plus its privileged seeding and audit paths.
type storeEnv struct {
	// Store is the store under test, as the restricted application role
	// would use it (tenant-scoped, subject to row-level security).
	Store feedback.Store
	// SeedReviewer registers reviewerID in tenantID with status
	// reviewerActive or reviewerDeleted, as 'crosscodexd admin reviewer add'
	// (and 'delete') would.
	SeedReviewer func(tenantID, reviewerID, status string)
	// SeedJob stores job j of tenantID and its mappings, as a pipeline run
	// would.
	SeedJob func(tenantID string, j jobSeed)
	// Determinations returns tenantID's determinations, read through the
	// privileged path: the store has no method that reads them.
	Determinations func(tenantID string) []feedback.Determination
}

// storeSetup returns a store for one spec. It runs inside a BeforeEach, so
// it should register teardown with DeferCleanup. Both tenants
// contractTenants(token) names must exist before it returns. Every seeding
// call precedes the spec's first Store call.
type storeSetup func(token string) storeEnv

// contractTenants returns the two tenant IDs the contract uses for the spec
// whose random token is token.
func contractTenants(token string) (a, b string) {
	return "feedback-a-" + token, "feedback-b-" + token
}

// contractTime is the base of every fixture time; all are whole seconds, so
// they survive the database's microsecond precision unchanged.
var contractTime = time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)

// storeContract registers the specs every feedback.Store must pass. It runs
// against fakeStore in the unit suite and against PGStore in the integration
// suite, so the two cannot drift. It covers tenant scope (every method
// refuses a missing or mismatched tenant and changes nothing; another
// tenant's rows are invisible), JobMappings (sorting, derived IDs, non-viable
// rows, missing, unfinished and other users' jobs), votes (round trip,
// latest per voter, every refusal leaving nothing written), Determine,
// MarkStale, MarkReprojected and projection (round trip, every refusal with
// its sentinel and the store unchanged).
func storeContract(setup storeSetup) {
	Describe("feedback.Store contract", func() {
		var (
			env                         storeEnv
			store                       feedback.Store
			tenA, tenB                  string
			ctxA, ctxB                  context.Context
			jobDone, jobRunning, jobOfB string
			mID                         string // the mapping of s1 to t1 in jobDone
		)
		const reviewer = "alice"

		BeforeEach(func() {
			raw := make([]byte, 6)
			_, err := rand.Read(raw)
			Expect(err).NotTo(HaveOccurred())
			token := hex.EncodeToString(raw)
			tenA, tenB = contractTenants(token)
			jobDone, jobRunning, jobOfB = "fb-"+token+"-done", "fb-"+token+"-running", "fb-"+token+"-b"
			mID = feedback.MappingID(jobDone, "s1", "t1")
			env = setup(token)
			store = env.Store
			env.SeedReviewer(tenA, reviewer, reviewerActive)
			env.SeedReviewer(tenB, reviewer, reviewerActive)
			env.SeedReviewer(tenA, "gone", reviewerDeleted)
			env.SeedJob(tenA, jobSeed{ID: jobDone, Status: "completed", CreatedBy: "analyst", Mappings: []feedback.Mapping{
				{SourceID: "s1", TargetID: "t2", RelationshipType: "SUBSET_OF", Confidence: 0.3, Viability: 0},
				{SourceID: "s1", TargetID: "t1", RelationshipType: "EQUIVALENT_TO", Confidence: 0.9, Viability: 1},
				{SourceID: "s0", TargetID: "t9", RelationshipType: "SUPERSET_OF", Confidence: 0.5, Viability: 0.5},
			}})
			env.SeedJob(tenA, jobSeed{ID: jobRunning, Status: "running", CreatedBy: "analyst", Mappings: []feedback.Mapping{
				{SourceID: "s1", TargetID: "t1", RelationshipType: "EQUIVALENT_TO", Confidence: 0.9, Viability: 1},
			}})
			env.SeedJob(tenB, jobSeed{ID: jobOfB, Status: "completed", CreatedBy: "analyst", Mappings: []feedback.Mapping{
				{SourceID: "s1", TargetID: "t1", RelationshipType: "EQUIVALENT_TO", Confidence: 0.9, Viability: 1},
			}})
			ctxA, err = tenant.WithTenant(context.Background(), tenA)
			Expect(err).NotTo(HaveOccurred())
			ctxB, err = tenant.WithTenant(context.Background(), tenB)
			Expect(err).NotTo(HaveOccurred())
		})

		vote := func(id, voter string, t feedback.VoteType, at time.Duration) feedback.Vote {
			v := feedback.Vote{ID: id, MappingID: mID, JobID: jobDone, SourceID: "s1", TargetID: "t1", Voter: voter,
				Type: t, Rationale: "read both controls\n\tclosely", Evidence: map[string]string{"control": "AC-1"},
				CreatedAt: contractTime.Add(at)}
			if t == feedback.VoteRefine {
				v.SuggestedType = "SUBSET_OF"
			}
			return v
		}
		determination := func(id string, outcome feedback.ReviewStatus, at time.Duration) feedback.Determination {
			return feedback.Determination{ID: id, MappingID: mID, JobID: jobDone, SourceID: "s1", TargetID: "t1",
				Outcome: outcome, DecidedBy: reviewer, Rationale: "checked the control text",
				Request: []byte(`{"determination":"` + id + `"}`), Attestation: []byte{0x00, 0xff, 0x10},
				DecidedAt: contractTime.Add(at)}
		}
		staleDetermination := func(id string, at time.Duration) feedback.Determination {
			d := determination(id, feedback.ReviewStale, at)
			d.DecidedBy = feedback.DecidedByReproject
			return d
		}
		reviewOf := func(d feedback.Determination, edges ...string) feedback.Review {
			return feedback.Review{MappingID: d.MappingID, JobID: d.JobID, SourceID: d.SourceID, TargetID: d.TargetID,
				Status: d.Outcome, DeterminationID: d.ID, DecidedAt: d.DecidedAt, EdgeIDs: edges}
		}
		determinations := func(tenantID string) []feedback.Determination {
			ds := env.Determinations(tenantID)
			slices.SortFunc(ds, func(a, b feedback.Determination) int {
				return cmp.Or(a.DecidedAt.Compare(b.DecidedAt), cmp.Compare(a.ID, b.ID))
			})
			return ds
		}
		votesA := func() []feedback.Vote {
			vs, err := store.LatestVotes(ctxA, tenA, jobDone)
			Expect(err).NotTo(HaveOccurred())
			return vs
		}
		reviewsA := func() []feedback.Review {
			rs, err := store.Reviews(ctxA, tenA)
			Expect(err).NotTo(HaveOccurred())
			return rs
		}

		Describe("tenant scope", func() {
			var seeded feedback.Determination

			BeforeEach(func() {
				Expect(store.RecordVote(ctxA, tenA, vote("v-1", reviewer, feedback.VoteAgree, 0))).To(Succeed())
				seeded = determination("d-1", feedback.ReviewInvalid, time.Minute)
				Expect(store.Determine(ctxA, tenA, seeded, "", reviewOf(seeded, "e-1"))).To(Succeed())
			})

			calls := map[string]func(ctx context.Context, tenantID string) error{
				"JobMappings": func(ctx context.Context, t string) error { _, err := store.JobMappings(ctx, t, jobDone); return err },
				"RecordVote": func(ctx context.Context, t string) error {
					return store.RecordVote(ctx, t, vote("v-scope", reviewer, feedback.VoteDisagree, time.Hour))
				},
				"LatestVotes": func(ctx context.Context, t string) error { _, err := store.LatestVotes(ctx, t, jobDone); return err },
				"Review":      func(ctx context.Context, t string) error { _, _, err := store.Review(ctx, t, mID); return err },
				"Determine": func(ctx context.Context, t string) error {
					d := determination("d-scope", feedback.ReviewValid, time.Hour)
					return store.Determine(ctx, t, d, seeded.ID, reviewOf(d))
				},
				"MarkStale": func(ctx context.Context, t string) error {
					return store.MarkStale(ctx, t, staleDetermination("d-scope", time.Hour), seeded.ID)
				},
				"MarkReprojected": func(ctx context.Context, t string) error {
					return store.MarkReprojected(ctx, t, mID, seeded.ID, []string{"x"})
				},
				"Unprojected": func(ctx context.Context, t string) error { _, err := store.Unprojected(ctx, t); return err },
				"Reviews":     func(ctx context.Context, t string) error { _, err := store.Reviews(ctx, t); return err },
				"MarkProjected": func(ctx context.Context, t string) error {
					return store.MarkProjected(ctx, t, mID, seeded.ID, []string{"x"})
				},
			}
			names := make([]string, 0, len(calls))
			for name := range calls {
				names = append(names, name)
			}
			slices.Sort(names)

			unchanged := func() {
				Expect(votesA()).To(HaveExactElements(HaveField("ID", "v-1")))
				Expect(reviewsA()).To(Equal([]feedback.Review{reviewOf(seeded, "e-1")}))
				Expect(determinations(tenA)).To(Equal([]feedback.Determination{seeded}))
				Expect(determinations(tenB)).To(BeEmpty())
			}
			for _, name := range names {
				It(name+" refuses a context without a tenant and changes nothing", func() {
					Expect(calls[name](context.Background(), tenA)).To(MatchError(tenant.ErrNoTenant))
					unchanged()
				})
				It(name+" refuses a tenant argument other than the context's and changes nothing", func() {
					Expect(calls[name](ctxB, tenA)).To(MatchError(ContainSubstring("does not match")))
					unchanged()
				})
			}

			It("hides one tenant's job, votes and reviews from another", func() {
				_, err := store.JobMappings(ctxB, tenB, jobDone)
				Expect(err).To(MatchError(feedback.ErrJobNotFound))
				vs, err := store.LatestVotes(ctxB, tenB, jobDone)
				Expect(err).NotTo(HaveOccurred())
				Expect(vs).To(BeEmpty())
				_, found, err := store.Review(ctxB, tenB, mID)
				Expect(err).NotTo(HaveOccurred())
				Expect(found).To(BeFalse())
				rs, err := store.Reviews(ctxB, tenB)
				Expect(err).NotTo(HaveOccurred())
				Expect(rs).To(BeEmpty())
			})

			It("does not let another tenant vote on, determine or project the mapping, and changes nothing", func() {
				Expect(store.RecordVote(ctxB, tenB, vote("v-cross", reviewer, feedback.VoteDisagree, time.Hour))).
					To(MatchError(feedback.ErrJobNotFound))
				d := determination("d-cross", feedback.ReviewValid, time.Hour)
				Expect(store.Determine(ctxB, tenB, d, seeded.ID, reviewOf(d))).To(MatchError(feedback.ErrJobNotFound))
				Expect(store.MarkStale(ctxB, tenB, staleDetermination("d-cross", time.Hour), seeded.ID)).
					To(MatchError(feedback.ErrReviewNotFound))
				Expect(store.MarkReprojected(ctxB, tenB, mID, seeded.ID, []string{"x"})).To(MatchError(feedback.ErrReviewNotFound))
				Expect(store.MarkProjected(ctxB, tenB, mID, seeded.ID, []string{"x"})).To(MatchError(feedback.ErrReviewNotFound))
				unchanged()
			})
		})

		Describe("JobMappings", func() {
			It("returns every synthesized mapping, viable or not, sorted by source then target, with derived IDs", func() {
				ms, err := store.JobMappings(ctxA, tenA, jobDone)
				Expect(err).NotTo(HaveOccurred())
				Expect(ms).To(Equal([]feedback.Mapping{
					{ID: feedback.MappingID(jobDone, "s0", "t9"), JobID: jobDone, SourceID: "s0", TargetID: "t9",
						RelationshipType: "SUPERSET_OF", Confidence: 0.5, Viability: 0.5},
					{ID: mID, JobID: jobDone, SourceID: "s1", TargetID: "t1",
						RelationshipType: "EQUIVALENT_TO", Confidence: 0.9, Viability: 1},
					{ID: feedback.MappingID(jobDone, "s1", "t2"), JobID: jobDone, SourceID: "s1", TargetID: "t2",
						RelationshipType: "SUBSET_OF", Confidence: 0.3, Viability: 0},
				}))
			})

			It("refuses a job the tenant does not have with ErrJobNotFound", func() {
				_, err := store.JobMappings(ctxA, tenA, "no-such-job")
				Expect(err).To(MatchError(feedback.ErrJobNotFound))
			})

			It("refuses a job that has not completed with JobNotCompletedError naming it and its status", func() {
				_, err := store.JobMappings(ctxA, tenA, jobRunning)
				var notDone *feedback.JobNotCompletedError
				Expect(err).To(BeAssignableToTypeOf(notDone))
				notDone = err.(*feedback.JobNotCompletedError)
				Expect(*notDone).To(Equal(feedback.JobNotCompletedError{JobID: jobRunning, Status: "running"}))
				Expect(err).To(MatchError(ContainSubstring("wait for it to complete")))
			})

			// Reviewers review jobs other users created: the job reads are
			// tenant-wide whatever user ctx carries.
			It("shows a job tenant-wide, to a context carrying its creator or another user", func() {
				for _, user := range []string{"analyst", "reviewer-bob"} {
					ms, err := store.JobMappings(tenant.WithUser(ctxA, user), tenA, jobDone)
					Expect(err).NotTo(HaveOccurred(), user)
					Expect(ms).To(HaveLen(3), user)
				}
			})

			It("lets a context carrying another user vote on and determine the job's mapping", func() {
				ctx := tenant.WithUser(ctxA, reviewer)
				Expect(store.RecordVote(ctx, tenA, vote("v-1", reviewer, feedback.VoteAgree, 0))).To(Succeed())
				d := determination("d-1", feedback.ReviewValid, time.Minute)
				Expect(store.Determine(ctx, tenA, d, "", reviewOf(d))).To(Succeed())
				Expect(votesA()).To(HaveLen(1))
				Expect(reviewsA()).To(Equal([]feedback.Review{reviewOf(d)}))
			})

			It("still hides another tenant's job from a context carrying a user", func() {
				_, err := store.JobMappings(tenant.WithUser(ctxB, "analyst"), tenB, jobDone)
				Expect(err).To(MatchError(feedback.ErrJobNotFound))
			})
		})

		Describe("RecordVote and LatestVotes", func() {
			It("round-trips every field", func() {
				v := vote("v-1", reviewer, feedback.VoteRefine, 0)
				Expect(store.RecordVote(ctxA, tenA, v)).To(Succeed())
				Expect(votesA()).To(Equal([]feedback.Vote{v}))
			})

			It("returns a vote without evidence with nil evidence", func() {
				v := vote("v-1", reviewer, feedback.VoteAgree, 0)
				v.Evidence, v.Rationale = nil, ""
				Expect(store.RecordVote(ctxA, tenA, v)).To(Succeed())
				Expect(votesA()).To(Equal([]feedback.Vote{v}))
			})

			It("truncates CreatedAt to the microsecond, as the driver sends it", func() {
				v := vote("v-1", reviewer, feedback.VoteAgree, 1900*time.Nanosecond)
				Expect(store.RecordVote(ctxA, tenA, v)).To(Succeed())
				Expect(votesA()).To(HaveExactElements(HaveField("CreatedAt", Equal(v.CreatedAt.Truncate(time.Microsecond)))))
			})

			It("returns each voter's latest vote per mapping, sorted by mapping then voter", func() {
				other := vote("v-other", "bob", feedback.VoteEscalate, 0)
				other.MappingID, other.SourceID, other.TargetID = feedback.MappingID(jobDone, "s0", "t9"), "s0", "t9"
				for _, v := range []feedback.Vote{
					vote("v-2", reviewer, feedback.VoteDisagree, time.Hour),
					vote("v-1", reviewer, feedback.VoteAgree, 0),
					vote("v-3", "bob", feedback.VoteAgree, 0),
					other,
				} {
					Expect(store.RecordVote(ctxA, tenA, v)).To(Succeed())
				}
				// MappingID(job, "s0", "t9") sorts before MappingID(job, "s1", "t1").
				Expect(votesA()).To(Equal([]feedback.Vote{
					other,
					vote("v-2", reviewer, feedback.VoteDisagree, time.Hour),
					vote("v-3", "bob", feedback.VoteAgree, 0),
				}))
			})

			It("breaks a tie on CreatedAt by the greater vote ID", func() {
				Expect(store.RecordVote(ctxA, tenA, vote("v-b", reviewer, feedback.VoteDisagree, 0))).To(Succeed())
				Expect(store.RecordVote(ctxA, tenA, vote("v-a", reviewer, feedback.VoteAgree, 0))).To(Succeed())
				Expect(votesA()).To(HaveExactElements(HaveField("ID", "v-b")))
			})

			It("keeps the votes of another job out", func() {
				Expect(store.RecordVote(ctxA, tenA, vote("v-1", reviewer, feedback.VoteAgree, 0))).To(Succeed())
				vs, err := store.LatestVotes(ctxA, tenA, jobRunning)
				Expect(err).NotTo(HaveOccurred())
				Expect(vs).To(BeEmpty())
			})

			DescribeTable("refuses the vote and records nothing",
				func(mutate func(*feedback.Vote), want error, msg string) {
					Expect(store.RecordVote(ctxA, tenA, vote("v-0", reviewer, feedback.VoteAgree, 0))).To(Succeed())
					v := vote("v-1", reviewer, feedback.VoteDisagree, time.Hour)
					mutate(&v)
					err := store.RecordVote(ctxA, tenA, v)
					Expect(err).To(MatchError(want))
					Expect(err).To(MatchError(ContainSubstring(msg)))
					Expect(err).To(MatchError(ContainSubstring("nothing was recorded")))
					Expect(votesA()).To(HaveExactElements(HaveField("ID", "v-0")))
				},
				Entry("a pair that is not one of the job's mappings",
					func(v *feedback.Vote) {
						v.TargetID, v.MappingID = "t2x", feedback.MappingID(v.JobID, v.SourceID, "t2x")
					},
					feedback.ErrMappingNotFound, "crosscodex results export"),
				Entry("an invalid vote", func(v *feedback.Vote) { v.Rationale = " " }, feedback.ErrInvalid, "need a rationale"),
				Entry("a mapping ID that does not name the pair", func(v *feedback.Vote) { v.MappingID = "other" },
					feedback.ErrInvalid, "does not name job"),
			)

			It("refuses a vote on a job the tenant does not have and records nothing", func() {
				v := vote("v-1", reviewer, feedback.VoteAgree, 0)
				v.JobID, v.MappingID = "no-such-job", feedback.MappingID("no-such-job", "s1", "t1")
				Expect(store.RecordVote(ctxA, tenA, v)).To(MatchError(feedback.ErrJobNotFound))
				vs, err := store.LatestVotes(ctxA, tenA, "no-such-job")
				Expect(err).NotTo(HaveOccurred())
				Expect(vs).To(BeEmpty())
			})

			It("refuses a vote on a job that has not completed and records nothing", func() {
				v := vote("v-1", reviewer, feedback.VoteAgree, 0)
				v.JobID, v.MappingID = jobRunning, feedback.MappingID(jobRunning, "s1", "t1")
				var notDone *feedback.JobNotCompletedError
				Expect(store.RecordVote(ctxA, tenA, v)).To(BeAssignableToTypeOf(notDone))
				vs, err := store.LatestVotes(ctxA, tenA, jobRunning)
				Expect(err).NotTo(HaveOccurred())
				Expect(vs).To(BeEmpty())
			})

			It("refuses a vote ID already stored and keeps the first", func() {
				Expect(store.RecordVote(ctxA, tenA, vote("v-1", reviewer, feedback.VoteAgree, 0))).To(Succeed())
				err := store.RecordVote(ctxA, tenA, vote("v-1", "bob", feedback.VoteDisagree, time.Hour))
				Expect(err).To(MatchError(ContainSubstring("already recorded")))
				Expect(err).To(MatchError(ContainSubstring("nothing was recorded")))
				Expect(votesA()).To(Equal([]feedback.Vote{vote("v-1", reviewer, feedback.VoteAgree, 0)}))
			})
		})

		Describe("Determine", func() {
			It("records the first determination byte for byte and the review unprojected", func() {
				d := determination("d-1", feedback.ReviewInvalid, time.Minute)
				Expect(store.Determine(ctxA, tenA, d, "", reviewOf(d, "e-1", "e-2"))).To(Succeed())
				Expect(determinations(tenA)).To(Equal([]feedback.Determination{d}))
				r, found, err := store.Review(ctxA, tenA, mID)
				Expect(err).NotTo(HaveOccurred())
				Expect(found).To(BeTrue())
				Expect(r).To(Equal(reviewOf(d, "e-1", "e-2")))
			})

			It("replaces the review with a later determination that quotes the current one", func() {
				d1 := determination("d-1", feedback.ReviewInvalid, time.Minute)
				Expect(store.Determine(ctxA, tenA, d1, "", reviewOf(d1, "e-1"))).To(Succeed())
				Expect(store.MarkProjected(ctxA, tenA, mID, d1.ID, []string{"e-1"})).To(Succeed())
				d2 := determination("d-2", feedback.ReviewValid, time.Hour)
				Expect(store.Determine(ctxA, tenA, d2, d1.ID, reviewOf(d2))).To(Succeed())
				Expect(reviewsA()).To(Equal([]feedback.Review{reviewOf(d2)}))
				Expect(determinations(tenA)).To(Equal([]feedback.Determination{d1, d2}))
			})

			It("truncates DecidedAt to the microsecond, as the driver sends it", func() {
				d := determination("d-1", feedback.ReviewValid, 1900*time.Nanosecond)
				Expect(store.Determine(ctxA, tenA, d, "", reviewOf(d))).To(Succeed())
				want := d.DecidedAt.Truncate(time.Microsecond)
				Expect(determinations(tenA)).To(HaveExactElements(HaveField("DecidedAt", Equal(want))))
				Expect(reviewsA()).To(HaveExactElements(HaveField("DecidedAt", Equal(want))))
			})

			Describe("refusals", func() {
				var first feedback.Determination

				BeforeEach(func() {
					first = determination("d-1", feedback.ReviewInvalid, time.Minute)
					Expect(store.Determine(ctxA, tenA, first, "", reviewOf(first, "e-1"))).To(Succeed())
				})

				DescribeTable("refuses the determination with a sentinel and changes nothing",
					func(build func() (feedback.Determination, string, feedback.Review), want error) {
						d, expectPrev, next := build()
						Expect(store.Determine(ctxA, tenA, d, expectPrev, next)).To(MatchError(want))
						Expect(reviewsA()).To(Equal([]feedback.Review{reviewOf(first, "e-1")}))
						Expect(determinations(tenA)).To(Equal([]feedback.Determination{first}))
					},
					Entry("a review that has another determination", func() (feedback.Determination, string, feedback.Review) {
						d := determination("d-2", feedback.ReviewValid, time.Hour)
						return d, "d-0", reviewOf(d)
					}, feedback.ErrReviewChanged),
					Entry("no review expected where one exists", func() (feedback.Determination, string, feedback.Review) {
						d := determination("d-2", feedback.ReviewValid, time.Hour)
						return d, "", reviewOf(d)
					}, feedback.ErrReviewChanged),
					Entry("an unregistered reviewer", func() (feedback.Determination, string, feedback.Review) {
						d := determination("d-2", feedback.ReviewValid, time.Hour)
						d.DecidedBy = "nobody"
						return d, first.ID, reviewOf(d)
					}, cleaning.ErrReviewerNotRegistered),
					Entry("a deleted reviewer", func() (feedback.Determination, string, feedback.Review) {
						d := determination("d-2", feedback.ReviewValid, time.Hour)
						d.DecidedBy = "gone"
						return d, first.ID, reviewOf(d)
					}, cleaning.ErrReviewerDeleted),
					Entry("a job the tenant does not have", func() (feedback.Determination, string, feedback.Review) {
						d := determination("d-2", feedback.ReviewValid, time.Hour)
						d.JobID, d.MappingID = "no-such-job", feedback.MappingID("no-such-job", "s1", "t1")
						return d, "", reviewOf(d)
					}, feedback.ErrJobNotFound),
					Entry("a pair that is not one of the job's mappings", func() (feedback.Determination, string, feedback.Review) {
						d := determination("d-2", feedback.ReviewValid, time.Hour)
						d.TargetID, d.MappingID = "t2x", feedback.MappingID(jobDone, "s1", "t2x")
						return d, "", reviewOf(d)
					}, feedback.ErrMappingNotFound),
					Entry("a blank rationale", func() (feedback.Determination, string, feedback.Review) {
						d := determination("d-2", feedback.ReviewValid, time.Hour)
						d.Rationale = "\n"
						return d, first.ID, reviewOf(d)
					}, feedback.ErrInvalid),
				)

				It("refuses a job that has not completed with JobNotCompletedError", func() {
					d := determination("d-2", feedback.ReviewValid, time.Hour)
					d.JobID, d.MappingID = jobRunning, feedback.MappingID(jobRunning, "s1", "t1")
					var notDone *feedback.JobNotCompletedError
					Expect(store.Determine(ctxA, tenA, d, "", reviewOf(d))).To(BeAssignableToTypeOf(notDone))
					Expect(determinations(tenA)).To(Equal([]feedback.Determination{first}))
				})

				DescribeTable("refuses the determination with an actionable message and changes nothing",
					func(build func() (feedback.Determination, string, feedback.Review), msg string) {
						d, expectPrev, next := build()
						err := store.Determine(ctxA, tenA, d, expectPrev, next)
						Expect(err).To(MatchError(ContainSubstring(msg)))
						Expect(err).To(MatchError(ContainSubstring("nothing was recorded")))
						Expect(reviewsA()).To(Equal([]feedback.Review{reviewOf(first, "e-1")}))
						Expect(determinations(tenA)).To(Equal([]feedback.Determination{first}))
					},
					Entry("a stale determination", func() (feedback.Determination, string, feedback.Review) {
						d := staleDetermination("d-2", time.Hour)
						return d, first.ID, reviewOf(d)
					}, "recorded only by MarkStale"),
					Entry("a next review for another determination", func() (feedback.Determination, string, feedback.Review) {
						d := determination("d-2", feedback.ReviewValid, time.Hour)
						next := reviewOf(d)
						next.DeterminationID = "d-x"
						return d, first.ID, next
					}, "next is not determination d-2's review"),
					Entry("a next review with another outcome", func() (feedback.Determination, string, feedback.Review) {
						d := determination("d-2", feedback.ReviewValid, time.Hour)
						next := reviewOf(d)
						next.Status = feedback.ReviewInvalid
						return d, first.ID, next
					}, "next is not determination d-2's review"),
					Entry("a determination ID already stored", func() (feedback.Determination, string, feedback.Review) {
						d := determination(first.ID, feedback.ReviewValid, time.Hour)
						return d, first.ID, reviewOf(d)
					}, "already recorded"),
					Entry("a changed review", func() (feedback.Determination, string, feedback.Review) {
						d := determination("d-2", feedback.ReviewValid, time.Hour)
						return d, "d-0", reviewOf(d)
					}, "its determination is now d-1"),
				)
			})
		})

		Describe("MarkStale", func() {
			var first feedback.Determination

			BeforeEach(func() {
				first = determination("d-1", feedback.ReviewInvalid, time.Minute)
				Expect(store.Determine(ctxA, tenA, first, "", reviewOf(first, "e-1"))).To(Succeed())
			})

			It("records the determination and marks the review stale and projected, keeping its edges", func() {
				d := staleDetermination("d-2", time.Hour)
				Expect(store.MarkStale(ctxA, tenA, d, first.ID)).To(Succeed())
				Expect(determinations(tenA)).To(Equal([]feedback.Determination{first, d}))
				want := reviewOf(d, "e-1")
				want.GraphApplied = true
				Expect(reviewsA()).To(Equal([]feedback.Review{want}))
			})

			DescribeTable("refuses the marking and changes nothing",
				func(build func() (feedback.Determination, string), want error, msg string) {
					d, expectPrev := build()
					err := store.MarkStale(ctxA, tenA, d, expectPrev)
					if want != nil {
						Expect(err).To(MatchError(want))
					}
					Expect(err).To(MatchError(ContainSubstring(msg)))
					Expect(reviewsA()).To(Equal([]feedback.Review{reviewOf(first, "e-1")}))
					Expect(determinations(tenA)).To(Equal([]feedback.Determination{first}))
				},
				Entry("a reviewer's determination", func() (feedback.Determination, string) {
					return determination("d-2", feedback.ReviewValid, time.Hour), first.ID
				}, nil, "MarkStale records only re-projection's"),
				Entry("a changed review", func() (feedback.Determination, string) {
					return staleDetermination("d-2", time.Hour), "d-0"
				}, feedback.ErrReviewChanged, "Reload the mapping"),
				Entry("a mapping without a review", func() (feedback.Determination, string) {
					d := staleDetermination("d-2", time.Hour)
					d.TargetID, d.MappingID = "t2", feedback.MappingID(jobDone, "s1", "t2")
					return d, ""
				}, feedback.ErrReviewNotFound, "review not found"),
				Entry("a determination ID already stored", func() (feedback.Determination, string) {
					return staleDetermination(first.ID, time.Hour), first.ID
				}, nil, "already recorded"),
				Entry("an invalid determination", func() (feedback.Determination, string) {
					d := staleDetermination("d-2", time.Hour)
					d.Attestation = nil
					return d, first.ID
				}, feedback.ErrInvalid, "an attestation are required"),
			)
		})

		Describe("MarkReprojected", func() {
			var first feedback.Determination

			BeforeEach(func() {
				first = determination("d-1", feedback.ReviewInvalid, time.Minute)
				Expect(store.Determine(ctxA, tenA, first, "", reviewOf(first, "e-1"))).To(Succeed())
			})

			It("replaces the edge IDs of a projected review and keeps every other field", func() {
				Expect(store.MarkProjected(ctxA, tenA, mID, first.ID, []string{"e-1"})).To(Succeed())
				Expect(store.MarkReprojected(ctxA, tenA, mID, first.ID, []string{"e-9", "e-8"})).To(Succeed())
				want := reviewOf(first, "e-9", "e-8")
				want.GraphApplied = true
				Expect(reviewsA()).To(Equal([]feedback.Review{want}))
				Expect(determinations(tenA)).To(Equal([]feedback.Determination{first}))
			})

			DescribeTable("refuses the write and changes nothing",
				func(project, unknownMapping bool, expect string, want error, msg string) {
					before := reviewOf(first, "e-1")
					if project {
						Expect(store.MarkProjected(ctxA, tenA, mID, first.ID, []string{"e-1"})).To(Succeed())
						before.GraphApplied = true
					}
					mappingID := mID
					if unknownMapping {
						mappingID = "no-such-mapping"
					}
					err := store.MarkReprojected(ctxA, tenA, mappingID, expect, []string{"x"})
					Expect(err).To(MatchError(want))
					Expect(err).To(MatchError(ContainSubstring(msg)))
					Expect(reviewsA()).To(Equal([]feedback.Review{before}))
				},
				Entry("a review with an outstanding graph change", false, false, "d-1", feedback.ErrUnprojected, "Record any determination"),
				Entry("a changed review", true, false, "d-0", feedback.ErrReviewChanged, "its determination is now d-1"),
				Entry("a mapping without a review", true, true, "d-1", feedback.ErrReviewNotFound, "review not found"),
			)
		})

		Describe("Unprojected, Reviews and MarkProjected", func() {
			It("lists a determined review until it is projected, then with its new edges", func() {
				d := determination("d-1", feedback.ReviewInvalid, time.Minute)
				Expect(store.Determine(ctxA, tenA, d, "", reviewOf(d, "e-1"))).To(Succeed())
				un, err := store.Unprojected(ctxA, tenA)
				Expect(err).NotTo(HaveOccurred())
				Expect(un).To(Equal([]feedback.Review{reviewOf(d, "e-1")}))

				Expect(store.MarkProjected(ctxA, tenA, mID, d.ID, []string{"e-2"})).To(Succeed())
				un, err = store.Unprojected(ctxA, tenA)
				Expect(err).NotTo(HaveOccurred())
				Expect(un).To(BeEmpty())
				want := reviewOf(d, "e-2")
				want.GraphApplied = true
				Expect(reviewsA()).To(Equal([]feedback.Review{want}))
			})

			It("returns every review sorted by mapping ID", func() {
				d1 := determination("d-1", feedback.ReviewValid, time.Minute)
				d1.TargetID, d1.MappingID = "t2", feedback.MappingID(jobDone, "s1", "t2")
				d2 := determination("d-2", feedback.ReviewValid, time.Minute)
				d2.SourceID, d2.TargetID, d2.MappingID = "s0", "t9", feedback.MappingID(jobDone, "s0", "t9")
				d3 := determination("d-3", feedback.ReviewInvalid, time.Minute)
				for _, d := range []feedback.Determination{d1, d2, d3} {
					Expect(store.Determine(ctxA, tenA, d, "", reviewOf(d))).To(Succeed())
				}
				ids := []string{}
				for _, r := range reviewsA() {
					ids = append(ids, r.MappingID)
				}
				Expect(ids).To(Equal(slices.Sorted(slices.Values([]string{d1.MappingID, d2.MappingID, d3.MappingID}))))
			})

			It("refuses to mark a mapping without a review", func() {
				err := store.MarkProjected(ctxA, tenA, "no-such-mapping", "d-1", nil)
				Expect(err).To(MatchError(feedback.ErrReviewNotFound))
				Expect(reviewsA()).To(BeEmpty())
			})

			// A projector read D1 as unprojected and wrote its edges, then D2
			// was recorded: marking D1 projected must not mark D2 projected.
			It("refuses to mark projected a determination the review no longer has, keeping the later one unprojected", func() {
				d1 := determination("d-1", feedback.ReviewInvalid, time.Minute)
				Expect(store.Determine(ctxA, tenA, d1, "", reviewOf(d1, "e-1"))).To(Succeed())
				d2 := determination("d-2", feedback.ReviewValid, time.Hour)
				Expect(store.Determine(ctxA, tenA, d2, d1.ID, reviewOf(d2, "e-1"))).To(Succeed())
				err := store.MarkProjected(ctxA, tenA, mID, d1.ID, []string{"e-1"})
				Expect(err).To(MatchError(feedback.ErrReviewChanged))
				Expect(err).To(MatchError(ContainSubstring("its determination is now d-2")))
				un, err := store.Unprojected(ctxA, tenA)
				Expect(err).NotTo(HaveOccurred())
				Expect(un).To(Equal([]feedback.Review{reviewOf(d2, "e-1")}))
			})
		})

		Describe("the edge_ids bound", func() {
			// A lowered bound: two short edge IDs encode within it, three do not.
			BeforeEach(func() { DeferCleanup(feedback.SetStoredEdgeIDsLimit(len(`["e-1","e-2"]`))) })

			over := []string{"e-1", "e-2", "e-3"}
			var first feedback.Determination

			BeforeEach(func() {
				first = determination("d-1", feedback.ReviewInvalid, time.Minute)
				Expect(store.Determine(ctxA, tenA, first, "", reviewOf(first, "e-1", "e-2"))).To(Succeed())
			})

			expectRefused := func(err error, before feedback.Review) {
				Expect(err).To(MatchError(feedback.ErrEdgeIDsTooLarge))
				Expect(err).To(MatchError(ContainSubstring("nothing was recorded")))
				Expect(reviewsA()).To(Equal([]feedback.Review{before}))
			}

			It("refuses a determination whose review would hold too many edge IDs", func() {
				d := determination("d-2", feedback.ReviewValid, time.Hour)
				expectRefused(store.Determine(ctxA, tenA, d, first.ID, reviewOf(d, over...)), reviewOf(first, "e-1", "e-2"))
				Expect(determinations(tenA)).To(Equal([]feedback.Determination{first}))
			})

			It("refuses a projection with too many edge IDs", func() {
				expectRefused(store.MarkProjected(ctxA, tenA, mID, first.ID, over), reviewOf(first, "e-1", "e-2"))
			})

			It("refuses a re-projection with too many edge IDs", func() {
				Expect(store.MarkProjected(ctxA, tenA, mID, first.ID, []string{"e-1"})).To(Succeed())
				before := reviewOf(first, "e-1")
				before.GraphApplied = true
				expectRefused(store.MarkReprojected(ctxA, tenA, mID, first.ID, over), before)
			})
		})
	})
}
