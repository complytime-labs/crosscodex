package feedback_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/internal/feedback"
	"github.com/complytime-labs/crosscodex/pkg/cleaning"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/telemetry/telemetrytest"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

var _ = Describe("fakeStore", func() {
	storeContract(func(string) storeEnv {
		f := &fakeStore{}
		return storeEnv{
			Store: f,
			SeedReviewer: func(tenantID, reviewerID, status string) {
				if f.Reviewers == nil {
					f.Reviewers = map[string]map[string]string{}
				}
				if f.Reviewers[tenantID] == nil {
					f.Reviewers[tenantID] = map[string]string{}
				}
				f.Reviewers[tenantID][reviewerID] = status
			},
			SeedJob: func(tenantID string, j jobSeed) {
				if f.Jobs == nil {
					f.Jobs = map[string][]jobSeed{}
				}
				f.Jobs[tenantID] = append(f.Jobs[tenantID], j)
			},
			Determinations: f.Determinations,
		}
	})
})

// validVote returns a vote every Validate accepts; specs change one field.
func validVote() feedback.Vote {
	return feedback.Vote{ID: "v-1", MappingID: feedback.MappingID("job-1", "s1", "t1"), JobID: "job-1",
		SourceID: "s1", TargetID: "t1", Voter: "alice", Type: feedback.VoteDisagree, Rationale: "the scopes differ",
		Evidence: map[string]string{"control": "AC-1"}, CreatedAt: contractTime}
}

// validDetermination returns a determination every Validate accepts; specs
// change one field.
func validDetermination() feedback.Determination {
	return feedback.Determination{ID: "d-1", MappingID: feedback.MappingID("job-1", "s1", "t1"), JobID: "job-1",
		SourceID: "s1", TargetID: "t1", Outcome: feedback.ReviewInvalid, DecidedBy: "alice", Rationale: "scopes differ",
		Request: []byte("{}"), Attestation: []byte("sig"), DecidedAt: contractTime}
}

// evidenceOf returns n distinct evidence entries.
func evidenceOf(n int) map[string]string {
	out := map[string]string{}
	for i := range n {
		out[fmt.Sprintf("k%d", i)] = "v"
	}
	return out
}

var _ = Describe("Vote.Validate", func() {
	DescribeTable("accepts",
		func(mutate func(*feedback.Vote)) {
			v := validVote()
			mutate(&v)
			Expect(v.Validate()).To(Succeed())
		},
		Entry("an agree vote without a rationale", func(v *feedback.Vote) { v.Type, v.Rationale = feedback.VoteAgree, "" }),
		Entry("a refine vote with a suggested type", func(v *feedback.Vote) { v.Type, v.SuggestedType = feedback.VoteRefine, "SUBSET_OF" }),
		Entry("a rationale with newlines and tabs", func(v *feedback.Vote) { v.Rationale = "one\n\ttwo" }),
		Entry("a rationale of exactly 4096 bytes", func(v *feedback.Vote) { v.Rationale = strings.Repeat("é", 2048) }),
		Entry("32 evidence entries", func(v *feedback.Vote) { v.Evidence = evidenceOf(32) }),
		Entry("an evidence key of 128 bytes and a value of 1024", func(v *feedback.Vote) {
			v.Evidence = map[string]string{strings.Repeat("k", 128): strings.Repeat("v", 1024)}
		}),
		Entry("no evidence", func(v *feedback.Vote) { v.Evidence = nil }),
		Entry("a voter, vote ID and job ID of 256 bytes", func(v *feedback.Vote) {
			v.Voter, v.ID, v.JobID = strings.Repeat("a", 256), strings.Repeat("v", 256), strings.Repeat("j", 256)
			v.MappingID = feedback.MappingID(v.JobID, v.SourceID, v.TargetID)
		}),
	)

	It("accepts every relationship type the proto defines, without its prefix, as a refine vote's suggested type", func() {
		for name, n := range pb.RelationshipType_value {
			if n == int32(pb.RelationshipType_RELATIONSHIP_TYPE_UNSPECIFIED) {
				continue
			}
			v := validVote()
			v.Type, v.SuggestedType = feedback.VoteRefine, strings.TrimPrefix(name, "RELATIONSHIP_TYPE_")
			Expect(v.Validate()).To(Succeed(), name)
		}
	})

	DescribeTable("refuses with ErrInvalid, saying what to change",
		func(mutate func(*feedback.Vote), msg string) {
			v := validVote()
			mutate(&v)
			err := v.Validate()
			Expect(err).To(MatchError(feedback.ErrInvalid))
			Expect(err).To(MatchError(ContainSubstring(msg)))
			Expect(err).To(MatchError(ContainSubstring("nothing was recorded")))
		},
		Entry("no vote ID", func(v *feedback.Vote) { v.ID = "" }, "a vote ID and a voter are required"),
		Entry("no voter", func(v *feedback.Vote) { v.Voter = "" }, "a vote ID and a voter are required"),
		Entry("no job ID", func(v *feedback.Vote) { v.JobID = "" }, "the job ID is required"),
		Entry("a mapping ID of another pair", func(v *feedback.Vote) { v.MappingID = feedback.MappingID("job-1", "s1", "t2") },
			"does not name job job-1's mapping of s1 to t1"),
		Entry("an unknown type", func(v *feedback.Vote) { v.Type = "AGREE" }, `unknown vote type "AGREE"`),
		Entry("a refine vote without a suggested type", func(v *feedback.Vote) { v.Type = feedback.VoteRefine },
			"must suggest the relationship type"),
		Entry("a suggested type on a disagree vote", func(v *feedback.Vote) { v.SuggestedType = "SUBSET_OF" },
			"only a refine vote suggests"),
		Entry("a disagree vote without a rationale", func(v *feedback.Vote) { v.Rationale = "" }, "disagree votes need a rationale: say why"),
		Entry("a blank escalate rationale", func(v *feedback.Vote) { v.Type, v.Rationale = feedback.VoteEscalate, " \n" },
			"escalate votes need a rationale"),
		Entry("a rationale over 4096 bytes", func(v *feedback.Vote) { v.Rationale = strings.Repeat("x", 4097) },
			"rationale is 4097 bytes, over the 4096-byte limit"),
		Entry("a rationale with a control character", func(v *feedback.Vote) { v.Rationale = "a\x1b[2Jb" },
			"only newlines and tabs are allowed"),
		Entry("a rationale that is not UTF-8", func(v *feedback.Vote) { v.Rationale = "a\xffb" }, "rationale is not valid UTF-8"),
		Entry("33 evidence entries", func(v *feedback.Vote) { v.Evidence = evidenceOf(33) }, "33 entries, over the limit of 32"),
		Entry("an evidence key over 128 bytes", func(v *feedback.Vote) { v.Evidence = map[string]string{strings.Repeat("k", 129): "v"} },
			"129 bytes, over the 128-byte limit"),
		Entry("an evidence value over 1024 bytes", func(v *feedback.Vote) { v.Evidence = map[string]string{"k": strings.Repeat("v", 1025)} },
			"1025 bytes, over the 1024-byte limit"),
		Entry("a newline in an evidence value", func(v *feedback.Vote) { v.Evidence = map[string]string{"k": "a\nb"} },
			`evidence value of "k" holds control character U+000A`),
		Entry("a control character in the voter", func(v *feedback.Vote) { v.Voter = "al\x00ice" }, "voter holds control character U+0000"),
		Entry("no creation time", func(v *feedback.Vote) { v.CreatedAt = time.Time{} }, "a creation time is required"),
		Entry("a voter over 256 bytes", func(v *feedback.Vote) { v.Voter = strings.Repeat("a", 257) },
			"voter is 257 bytes, over the 256-byte limit"),
		Entry("a vote ID over 256 bytes", func(v *feedback.Vote) { v.ID = strings.Repeat("v", 257) },
			"vote ID is 257 bytes, over the 256-byte limit"),
		Entry("a source ID over 256 bytes", func(v *feedback.Vote) {
			v.SourceID = strings.Repeat("s", 257)
			v.MappingID = feedback.MappingID(v.JobID, v.SourceID, v.TargetID)
		}, "source ID is 257 bytes, over the 256-byte limit"),
		Entry("a suggested type the proto does not define", func(v *feedback.Vote) {
			v.Type, v.SuggestedType = feedback.VoteRefine, "EQUIVALENT_TO"
		}, `unknown relationship type "EQUIVALENT_TO"; use one of`),
		Entry("the unspecified relationship type", func(v *feedback.Vote) {
			v.Type, v.SuggestedType = feedback.VoteRefine, "UNSPECIFIED"
		}, `unknown relationship type "UNSPECIFIED"`),
		Entry("a suggested type with the enum prefix", func(v *feedback.Vote) {
			v.Type, v.SuggestedType = feedback.VoteRefine, "RELATIONSHIP_TYPE_SUBSET_OF"
		}, `unknown relationship type "RELATIONSHIP_TYPE_SUBSET_OF"`),
	)
})

var _ = Describe("Determination.Validate", func() {
	It("accepts re-projection's stale determination", func() {
		d := validDetermination()
		d.Outcome, d.DecidedBy = feedback.ReviewStale, feedback.DecidedByReproject
		Expect(d.Validate()).To(Succeed())
	})

	DescribeTable("refuses with ErrInvalid, saying what to change",
		func(mutate func(*feedback.Determination), msg string) {
			d := validDetermination()
			mutate(&d)
			err := d.Validate()
			Expect(err).To(MatchError(feedback.ErrInvalid))
			Expect(err).To(MatchError(ContainSubstring(msg)))
			Expect(err).To(MatchError(ContainSubstring("nothing was recorded")))
		},
		Entry("no determination ID", func(d *feedback.Determination) { d.ID = "" }, "a determination ID and a decider are required"),
		Entry("no decider", func(d *feedback.Determination) { d.DecidedBy = "" }, "a determination ID and a decider are required"),
		Entry("no source ID", func(d *feedback.Determination) { d.SourceID = "" }, "the source ID is required"),
		Entry("a mapping ID of another job", func(d *feedback.Determination) { d.MappingID = feedback.MappingID("job-2", "s1", "t1") },
			"does not name job job-1's mapping"),
		Entry("an unknown outcome", func(d *feedback.Determination) { d.Outcome = "retired" }, `unknown outcome "retired"`),
		Entry("a stale outcome by a reviewer", func(d *feedback.Determination) { d.Outcome = feedback.ReviewStale },
			"only re-projection records a \"stale\" determination"),
		Entry("re-projection deciding valid", func(d *feedback.Determination) { d.DecidedBy = feedback.DecidedByReproject },
			"only re-projection records a \"stale\" determination"),
		Entry("a blank rationale", func(d *feedback.Determination) { d.Rationale = "\t\n" }, "a rationale is required"),
		Entry("a rationale over 4096 bytes", func(d *feedback.Determination) { d.Rationale = strings.Repeat("x", 4097) },
			"over the 4096-byte limit"),
		Entry("a control character in the rationale", func(d *feedback.Determination) { d.Rationale = "a\rb" },
			"holds control character U+000D"),
		Entry("no request", func(d *feedback.Determination) { d.Request = nil }, "a request and an attestation are required"),
		Entry("an empty attestation", func(d *feedback.Determination) { d.Attestation = []byte{} },
			"a request and an attestation are required"),
		Entry("no decision time", func(d *feedback.Determination) { d.DecidedAt = time.Time{} }, "a decision time is required"),
		Entry("a decider over 256 bytes", func(d *feedback.Determination) { d.DecidedBy = strings.Repeat("a", 257) },
			"decider is 257 bytes, over the 256-byte limit"),
		Entry("a determination ID over 256 bytes", func(d *feedback.Determination) { d.ID = strings.Repeat("d", 257) },
			"determination ID is 257 bytes, over the 256-byte limit"),
		Entry("a target ID over 256 bytes", func(d *feedback.Determination) {
			d.TargetID = strings.Repeat("t", 257)
			d.MappingID = feedback.MappingID(d.JobID, d.SourceID, d.TargetID)
		}, "target ID is 257 bytes, over the 256-byte limit"),
	)
})

// Specs for what only PGStore does, over a scripted connection: its mapping
// of database errors to sentinels and messages, and its telemetry. Behaviour
// shared with fakeStore is in storeContract, which
// store_integration_bdd_test.go runs against Postgres.
var _ = Describe("PGStore", func() {
	const tenantID = "tenant-a"
	var (
		ctx   context.Context
		tx    *dbtest.Tx
		store *feedback.PGStore
		d     feedback.Determination
		next  feedback.Review
	)

	BeforeEach(func() {
		var err error
		ctx, err = tenant.WithTenant(context.Background(), tenantID)
		Expect(err).NotTo(HaveOccurred())
		tx = &dbtest.Tx{}
		store = feedback.NewPGStore(&dbtest.Conn{Tx: tx})
		d = validDetermination()
		next = feedback.Review{MappingID: d.MappingID, JobID: d.JobID, SourceID: d.SourceID, TargetID: d.TargetID,
			Status: d.Outcome, DeterminationID: d.ID, DecidedAt: d.DecidedAt, EdgeIDs: []string{"e-1"}}
	})

	// writes returns the recorded statements that write a determination or
	// a review.
	writes := func() []dbtest.Statement {
		var out []dbtest.Statement
		for _, s := range tx.Execs() {
			if strings.Contains(s.Query, "mapping_determinations") || strings.Contains(s.Query, "mapping_reviews") {
				out = append(out, s)
			}
		}
		return out
	}

	DescribeTable("maps the reviewer check's SQLSTATE to a sentinel with the command that fixes it",
		func(code string, want error, command string) {
			tx.ExecFunc = func(_ context.Context, query string, _ ...any) error {
				if strings.Contains(query, "require_active_reviewer") {
					return &pgconn.PgError{Code: code}
				}
				return nil
			}
			err := store.Determine(ctx, tenantID, d, "", next)
			Expect(err).To(MatchError(want))
			Expect(err).To(MatchError(ContainSubstring("nothing was recorded")))
			Expect(err).To(MatchError(ContainSubstring(command)))
			Expect(writes()).To(BeEmpty())
			Expect(tx.Rollbacks()).To(Equal(1))
			Expect(tx.Commits()).To(BeZero())
		},
		Entry("P0002 is ErrReviewerNotRegistered", "P0002", cleaning.ErrReviewerNotRegistered, "crosscodexd admin reviewer add"),
		Entry("55000 is ErrReviewerDeleted", "55000", cleaning.ErrReviewerDeleted, "crosscodexd admin reviewer restore"),
	)

	Describe("Determine of a mapping without a review", func() {
		BeforeEach(func() {
			// The job is completed, the pair is its mapping, and the lock
			// finds no review.
			tx.QueryRowFunc = func(_ context.Context, query string, _ ...any) db.Row {
				switch {
				case strings.Contains(query, "FROM jobs"):
					return dbtest.Row(nil, "completed")
				case strings.Contains(query, "FROM vote_summaries"):
					return dbtest.Row(nil, true)
				}
				return dbtest.Row(sql.ErrNoRows)
			}
		})

		It("refuses a first review another determination inserted since the lock found none with ErrReviewChanged", func() {
			tx.ExecFunc = func(_ context.Context, query string, _ ...any) error {
				if strings.Contains(query, "INSERT INTO mapping_reviews") {
					return &pgconn.PgError{Code: "23505"}
				}
				return nil
			}
			err := store.Determine(ctx, tenantID, d, "", next)
			Expect(err).To(MatchError(feedback.ErrReviewChanged))
			Expect(err).To(MatchError(ContainSubstring("Reload the mapping and try again")))
			Expect(tx.Rollbacks()).To(Equal(1))
			Expect(tx.Commits()).To(BeZero())
		})

		It("writes no edge IDs as an empty JSON array, which the cleaning state view reads as no edge change", func() {
			tx.ExecFunc = func(context.Context, string, ...any) error { return nil }
			next.EdgeIDs = nil
			Expect(store.Determine(ctx, tenantID, d, "", next)).To(Succeed())
			var edges []any
			for _, s := range tx.Execs() {
				if strings.Contains(s.Query, "INSERT INTO mapping_reviews") {
					edges = append(edges, s.Args[8])
				}
			}
			Expect(edges).To(Equal([]any{"[]"}))
			Expect(tx.Commits()).To(Equal(1))
		})
	})

	Describe("telemetry", func() {
		var tp *telemetrytest.TestProvider

		BeforeEach(func() {
			var err error
			tp, err = telemetrytest.NewTestProvider()
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { Expect(tp.Shutdown(context.Background())).To(Succeed()) })
			// A nil connection: every call below is refused before using it.
			store = feedback.NewPGStore(nil, feedback.WithStoreTelemetry(tp.TracerProvider(), tp.MeterProvider()))
		})

		counter := func(key, value string) int64 {
			m := telemetrytest.FindMetric(tp.GetMetrics(), "feedback.store.operations.total")
			Expect(m).NotTo(BeNil())
			sum, ok := m.Data.(metricdata.Sum[int64])
			Expect(ok).To(BeTrue())
			var total int64
			for _, dp := range sum.DataPoints {
				if v, ok := dp.Attributes.Value(attribute.Key(key)); ok && v.AsString() == value {
					total += dp.Value
				}
			}
			return total
		}

		DescribeTable("traces, counts and times every method, including a refused call",
			func(op string, call func(ctx context.Context) error) {
				ctxB, err := tenant.WithTenant(context.Background(), "tenant-b")
				Expect(err).NotTo(HaveOccurred())
				Expect(call(ctxB)).To(MatchError(ContainSubstring("does not match")))
				span := telemetrytest.FindSpan(tp.GetSpans(), "feedback.store."+op)
				Expect(span).NotTo(BeNil())
				Expect(span.Status().Code).To(Equal(codes.Error))
				v, ok := telemetrytest.SpanAttribute(span, "tenant.id")
				Expect(ok).To(BeTrue())
				Expect(v.AsString()).To(Equal(tenantID))
				Expect(counter("operation", op)).To(Equal(int64(1)))
				Expect(counter("status", "error")).To(Equal(int64(1)))
				m := telemetrytest.FindMetric(tp.GetMetrics(), "feedback.store.operation.duration_ms")
				Expect(m).NotTo(BeNil())
				Expect(telemetrytest.HistogramCount(m)).To(Equal(uint64(1)))
			},
			Entry("JobMappings", "JobMappings", func(c context.Context) error { _, err := store.JobMappings(c, tenantID, "j"); return err }),
			Entry("RecordVote", "RecordVote", func(c context.Context) error { return store.RecordVote(c, tenantID, validVote()) }),
			Entry("LatestVotes", "LatestVotes", func(c context.Context) error { _, err := store.LatestVotes(c, tenantID, "j"); return err }),
			Entry("Review", "Review", func(c context.Context) error { _, _, err := store.Review(c, tenantID, "m"); return err }),
			Entry("Determine", "Determine", func(c context.Context) error { return store.Determine(c, tenantID, d, "", next) }),
			Entry("MarkStale", "MarkStale", func(c context.Context) error {
				s := validDetermination()
				s.Outcome, s.DecidedBy = feedback.ReviewStale, feedback.DecidedByReproject
				return store.MarkStale(c, tenantID, s, "d-0")
			}),
			Entry("MarkReprojected", "MarkReprojected", func(c context.Context) error {
				return store.MarkReprojected(c, tenantID, "m", "d", nil)
			}),
			Entry("Unprojected", "Unprojected", func(c context.Context) error { _, err := store.Unprojected(c, tenantID); return err }),
			Entry("Reviews", "Reviews", func(c context.Context) error { _, err := store.Reviews(c, tenantID); return err }),
			Entry("MarkProjected", "MarkProjected", func(c context.Context) error { return store.MarkProjected(c, tenantID, "m", "d", nil) }),
		)
	})
})
