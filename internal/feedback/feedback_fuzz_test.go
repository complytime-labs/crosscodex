package feedback_test

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/complytime-labs/crosscodex/internal/feedback"
)

// FuzzVoteValidate checks that every vote Validate accepts honours the
// spec's limits: rationale, evidence and IDs are UTF-8 text without control
// characters (newlines and tabs allowed in the rationale only), within their
// byte bounds, and the mapping ID names the job's pair.
func FuzzVoteValidate(f *testing.F) {
	f.Add("v-1", "job-1", "s1", "t1", "alice", "disagree", "", "scopes differ", "control", "AC-1")
	f.Add("v-1", "job-1", "s1", "t1", "alice", "agree", "", "", "", "")
	f.Add("v-1", "job_1", "s%1", "t1", "alice", "refine", "SUBSET_OF", "line\n\ttwo", "k", "v")
	f.Add("v-1", "job-1", "s1", "t1", "alice", "refine", "", "no suggestion", "k", "v")
	f.Add("v-1", "job-1", "s1", "t1", "alice", "escalate", "", strings.Repeat("x", 4097), "k", "v")
	f.Add("v-1", "job-1", "s1", "t1", "al\x00ice", "disagree", "", "a\x1b[2Jb", "k\n", "v\xff")
	f.Add("", "", "", "", "", "", "", "", "", "")
	f.Fuzz(func(t *testing.T, id, job, src, tgt, voter, voteType, suggested, rationale, key, value string) {
		v := feedback.Vote{ID: id, MappingID: feedback.MappingID(job, src, tgt), JobID: job, SourceID: src, TargetID: tgt,
			Voter: voter, Type: feedback.VoteType(voteType), SuggestedType: suggested, Rationale: rationale,
			CreatedAt: time.Unix(1, 0)}
		if key != "" || value != "" {
			v.Evidence = map[string]string{key: value}
		}
		if v.Validate() != nil {
			return
		}
		text := func(name, s string, multiline bool) {
			if !utf8.ValidString(s) {
				t.Fatalf("accepted a %s that is not UTF-8: %q", name, s)
			}
			for _, r := range s {
				whitespace := multiline && (r == '\n' || r == '\t')
				if unicode.IsControl(r) && !whitespace {
					t.Fatalf("accepted a %s with control character %U: %q", name, r, s)
				}
			}
		}
		for name, s := range map[string]string{"vote ID": id, "job ID": job, "source ID": src, "target ID": tgt,
			"voter": voter, "suggested type": suggested, "evidence key": key, "evidence value": value} {
			text(name, s, false)
		}
		text("rationale", rationale, true)
		if id == "" || job == "" || src == "" || tgt == "" || voter == "" {
			t.Fatalf("accepted a vote with an empty ID or voter: %+v", v)
		}
		if len(rationale) > 4096 || len(key) > 128 || len(value) > 1024 {
			t.Fatalf("accepted a vote over a size bound: rationale %d, key %d, value %d bytes", len(rationale), len(key), len(value))
		}
		if (v.Type == feedback.VoteRefine) != (suggested != "") {
			t.Fatalf("accepted a %q vote with suggested type %q", v.Type, suggested)
		}
		if v.Type != feedback.VoteAgree && strings.TrimSpace(rationale) == "" {
			t.Fatalf("accepted a %q vote without a rationale", v.Type)
		}
	})
}

// FuzzDeterminationValidate checks that every determination Validate
// accepts honours the spec's limits: a non-blank rationale of at most 4096
// bytes, UTF-8 text without control characters (newlines and tabs allowed in
// the rationale only), IDs and decider of at most 256 bytes, stale decided by
// re-projection and by nothing else, and the mapping ID naming the job's pair.
func FuzzDeterminationValidate(f *testing.F) {
	f.Add("d-1", "job-1", "s1", "t1", "alice", "invalid", "scopes differ", []byte("{}"), []byte("sig"))
	f.Add("d-1", "job-1", "s1", "t1", "reproject", "stale", "edges gone\n\tafter rebuild", []byte("{}"), []byte("sig"))
	f.Add("d-1", "job-1", "s1", "t1", "alice", "stale", "reviewer claims stale", []byte("{}"), []byte("sig"))
	f.Add("d-1", "job_1", "s%1", "t1", "reproject", "valid", "re-projection deciding valid", []byte("{}"), []byte("sig"))
	f.Add("d-1", "job-1", "s1", "t1", "alice", "valid", " \n\t", []byte("{}"), []byte("sig"))
	f.Add(strings.Repeat("d", 257), "job-1", "s1", "t1", "al\x00ice", "valid", "a\x1b[2Jb\xff", []byte{}, []byte(nil))
	f.Add("", "", "", "", "", "", "", []byte(nil), []byte(nil))
	f.Fuzz(func(t *testing.T, id, job, src, tgt, decidedBy, outcome, rationale string, request, attestation []byte) {
		d := feedback.Determination{ID: id, MappingID: feedback.MappingID(job, src, tgt), JobID: job, SourceID: src,
			TargetID: tgt, Outcome: feedback.ReviewStatus(outcome), DecidedBy: decidedBy, Rationale: rationale,
			Request: request, Attestation: attestation, DecidedAt: time.Unix(1, 0)}
		if d.Validate() != nil {
			return
		}
		for name, s := range map[string]string{"determination ID": id, "job ID": job, "source ID": src,
			"target ID": tgt, "decider": decidedBy} {
			if s == "" || len(s) > 256 || !utf8.ValidString(s) || strings.IndexFunc(s, unicode.IsControl) >= 0 {
				t.Fatalf("accepted a %s that is empty, over 256 bytes, not UTF-8 or with a control character: %q", name, s)
			}
		}
		if !utf8.ValidString(rationale) || len(rationale) > 4096 || strings.TrimSpace(rationale) == "" {
			t.Fatalf("accepted a rationale that is not UTF-8, over 4096 bytes or blank: %q", rationale)
		}
		for _, r := range rationale {
			if unicode.IsControl(r) && r != '\n' && r != '\t' {
				t.Fatalf("accepted a rationale with control character %U", r)
			}
		}
		if (d.Outcome == feedback.ReviewStale) != (decidedBy == feedback.DecidedByReproject) {
			t.Fatalf("accepted outcome %q decided by %q", outcome, decidedBy)
		}
		if len(request) == 0 || len(attestation) == 0 {
			t.Fatal("accepted a determination without a request or an attestation")
		}
	})
}

// FuzzDecodePageToken checks that every review queue page token the decoder
// accepts names a non-empty (source, target) position that re-encodes and
// decodes to the same position. Tokens are untrusted: callers send them back
// verbatim, or not.
func FuzzDecodePageToken(f *testing.F) {
	f.Add(feedback.EncodePageToken("a1", "b1"))
	f.Add(feedback.EncodePageToken("ctrl/é", "t\"q"))
	f.Add("not-base64!")
	f.Add(base64.RawURLEncoding.EncodeToString([]byte(`{"s":""}`)))
	f.Add(base64.RawURLEncoding.EncodeToString([]byte(`[1,2]`)))
	f.Add(base64.RawURLEncoding.EncodeToString([]byte(`null`)))
	f.Add(base64.RawURLEncoding.EncodeToString([]byte(`{"s":"a","t":"b","s":"c"}`)))
	f.Add(strings.Repeat("A", 1<<16))
	f.Add("")
	f.Fuzz(func(t *testing.T, token string) {
		source, target, err := feedback.DecodePageToken(token)
		if err != nil {
			return
		}
		if source == "" || target == "" {
			t.Fatalf("accepted token %q with an empty position (%q, %q)", token, source, target)
		}
		s2, t2, err := feedback.DecodePageToken(feedback.EncodePageToken(source, target))
		if err != nil || s2 != source || t2 != target {
			t.Fatalf("position (%q, %q) re-encoded to (%q, %q), err %v", source, target, s2, t2, err)
		}
	})
}
