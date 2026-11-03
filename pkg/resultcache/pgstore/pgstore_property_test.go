package pgstore_test

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/resultcache"
	"github.com/complytime-labs/crosscodex/pkg/resultcache/pgstore"
)

var _ = Describe("Property Specifications", Ordered, func() {
	It("never puts a caller's value into SQL text: every value is a bound argument", func() {
		ctx := tenantCtx()
		rapid.Check(GinkgoT(), func(t *rapid.T) {
			hostile := rapid.StringMatching(`[a-z' ;()-]{1,20}`)
			e := resultcache.Entry{
				JobID: "x" + hostile.Draw(t, "job"), Analyzer: "x" + hostile.Draw(t, "analyzer"), TaskID: "x" + hostile.Draw(t, "task"),
				Attempt:         "x" + hostile.Draw(t, "attempt"),
				CacheKey:        rapid.StringMatching(`[0-9a-f]{64}`).Draw(t, "key"),
				Result:          []byte(fmt.Sprintf(`{"model":%q}`, hostile.Draw(t, "result"))),
				ResponseModel:   "x" + hostile.Draw(t, "model"),
				SourceCatalogID: "x" + hostile.Draw(t, "source"), TargetCatalogID: "x" + hostile.Draw(t, "target"),
			}
			tx := &dbtest.Tx{ExecFunc: func(context.Context, string, ...any) error { return nil }}
			s, err := pgstore.New(&dbtest.Conn{Tx: tx})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := s.Put(ctx, e); err != nil {
				t.Fatalf("Put: %v", err)
			}
			for _, st := range tx.Execs() {
				for _, v := range []string{e.JobID, e.Analyzer, e.Attempt, e.TaskID, e.CacheKey, e.ResponseModel, e.SourceCatalogID, e.TargetCatalogID} {
					if strings.Contains(st.Query, v) {
						t.Fatalf("SQL text contains the value %q: %s", v, st.Query)
					}
				}
			}
			if len(tx.Execs()) != 2 || tx.Commits() != 1 {
				t.Fatalf("got %d statements and %d commits, want 2 and 1", len(tx.Execs()), tx.Commits())
			}
		})
	})
})
