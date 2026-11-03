//go:build integration

package pipeline

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
	"github.com/google/uuid"
)

// seedEmbeddedCatalog writes a catalog of tenantID with one non-section
// control per entry of vectors, embedded with that entry's pgvector literal
// (superuser: RLS bypassed, tenant_id explicit). Offset 1 so a failure is
// reported at the caller's line.
func seedEmbeddedCatalog(ctx context.Context, idb *integrationDB, tenantID, catalogID, name, model string, vectors map[string]string) {
	ExpectWithOffset(1, idb.suPool.Exec(ctx, `
		INSERT INTO catalogs (catalog_id, tenant_id, name, version, source_type, object_path)
		VALUES ($1, $2, $3, 'v1', 'test', 'unused')
	`, catalogID, tenantID, name)).To(Succeed(), "seed catalog %s", catalogID)
	for id, vec := range vectors {
		ExpectWithOffset(1, idb.suPool.Exec(ctx, `
			INSERT INTO controls (tenant_id, control_id, catalog_id, identifier, title, statement, class)
			VALUES ($1, $2, $3, $2, 'Title', 'Statement', 'SC')
		`, tenantID, id, catalogID)).To(Succeed(), "seed control %s", id)
		seedEmbedding(ctx, idb, tenantID, catalogID, id, model, vec)
	}
}

// seedEmbedding writes one embeddings row (superuser: RLS bypassed). Offset 1
// so a failure is reported at the caller's line.
func seedEmbedding(ctx context.Context, idb *integrationDB, tenantID, catalogID, controlID, model, vec string) {
	ExpectWithOffset(1, idb.suPool.Exec(ctx, `
		INSERT INTO embeddings (catalog_id, control_id, model, vector, tenant_id)
		VALUES ($1, $2, $3, $4::vector, $5)
	`, catalogID, controlID, model, vec, tenantID)).To(Succeed(), "seed embedding %s under %s", controlID, catalogID)
}

// axisVector builds an embeddingDim pgvector literal that is zero except for
// the given leading components, so specs can choose exact cosine similarities.
func axisVector(components ...float64) string {
	parts := make([]string, embeddingDim)
	for i := range parts {
		parts[i] = "0"
		if i < len(components) {
			parts[i] = fmt.Sprintf("%g", components[i])
		}
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// similarityAt returns values[row][col] by control ID. Offset 1 so a failure
// is reported at the caller's line.
func similarityAt(ids []string, values [][]float32, row, col string) float32 {
	index := make(map[string]int, len(ids))
	for i, id := range ids {
		index[id] = i
	}
	ri, rok := index[row]
	ci, cok := index[col]
	ExpectWithOffset(1, rok && cok).To(BeTrue(), "matrix ids %v lack %s or %s", ids, row, col)
	return values[ri][ci]
}

var _ = Describe("Catalog readers integration", func() {
	It("reads one similarity matrix over the job's two catalogs and nothing else, as the restricted role", func() {
		ctx := context.Background()
		idb := openIntegrationDB(ctx, 2)

		tenantA := fmt.Sprintf("readers-a-%s", uuid.New().String())
		tenantB := fmt.Sprintf("readers-b-%s", uuid.New().String())
		Expect(db.EnsureTenant(ctx, idb.suPool, tenantA, "readers-a")).To(Succeed(), "ensure tenant A")
		Expect(db.EnsureTenant(ctx, idb.suPool, tenantB, "readers-b")).To(Succeed(), "ensure tenant B")

		// s1 = e0, s2 = e1, t1 = 0.6 e0 + 0.8 e1: every pair has its own
		// similarity (s1-t1 60, s2-t1 80, s1-s2 0), so a misindexed matrix
		// fails the value assertions below.
		const model = "readers-model"
		src, tgt, third, other := "cat-src-"+uuid.NewString(), "cat-tgt-"+uuid.NewString(), "cat-third-"+uuid.NewString(), "cat-other-"+uuid.NewString()
		s1, s2, t1 := src+"/s1", src+"/s2", tgt+"/t1"
		seedEmbeddedCatalog(ctx, idb, tenantA, src, "Source", model, map[string]string{s1: axisVector(1), s2: axisVector(0, 1)})
		seedEmbeddedCatalog(ctx, idb, tenantA, tgt, "Target", model, map[string]string{t1: axisVector(0.6, 0.8)})
		seedEmbeddedCatalog(ctx, idb, tenantA, third, "Third", model, map[string]string{third + "/x1": axisVector(1)})
		seedEmbeddedCatalog(ctx, idb, tenantB, other, "Other", model, map[string]string{other + "/o1": axisVector(1)})

		ctxA, err := tenant.WithTenant(ctx, tenantA)
		Expect(err).NotTo(HaveOccurred())
		reader := &pgEmbeddingsReader{db: idb.tenantConn}

		ids, values, err := reader.SimilarityMatrix(ctxA, tenantA, []string{src, tgt}, model)
		Expect(err).NotTo(HaveOccurred())
		Expect(ids).To(ConsistOf(s1, s2, t1), "both catalogs' controls, nothing from a third catalog")
		Expect(values).To(HaveLen(3))
		Expect(similarityAt(ids, values, s1, t1)).To(BeNumerically("~", 60, 0.01), "s1 -> t1 across catalogs")
		Expect(similarityAt(ids, values, t1, s2)).To(BeNumerically("~", 80, 0.01), "t1 -> s2 across catalogs")
		Expect(similarityAt(ids, values, s1, s2)).To(BeNumerically("~", 0, 0.01), "s1 -> s2 within the source")
		Expect(similarityAt(ids, values, t1, t1)).To(BeNumerically("~", 100, 0.01), "t1 with itself")

		// Another tenant's catalog is invisible even when named.
		ids, _, err = reader.SimilarityMatrix(ctxA, tenantA, []string{other}, model)
		Expect(err).NotTo(HaveOccurred())
		Expect(ids).To(BeEmpty(), "tenant A read tenant B's embeddings")
	})

	It("ignores an embedding left under a catalog its control no longer belongs to, as the restricted role", func() {
		ctx := context.Background()
		idb := openIntegrationDB(ctx, 2)

		tenantA := fmt.Sprintf("stale-a-%s", uuid.New().String())
		Expect(db.EnsureTenant(ctx, idb.suPool, tenantA, "stale-a")).To(Succeed(), "ensure tenant A")

		const model = "stale-model"
		src, tgt := "cat-src-"+uuid.NewString(), "cat-tgt-"+uuid.NewString()
		s1, t1 := src+"/s1", tgt+"/t1"
		seedEmbeddedCatalog(ctx, idb, tenantA, src, "Source", model, map[string]string{s1: axisVector(1)})
		seedEmbeddedCatalog(ctx, idb, tenantA, tgt, "Target", model, map[string]string{t1: axisVector(0.6, 0.8)})
		// s1 once belonged to the target catalog: the controls upsert moved it
		// to src and left this embedding behind (embeddings have no foreign
		// key to controls). Counted, it would make s1 -> t1 read 80.
		seedEmbedding(ctx, idb, tenantA, tgt, s1, model, axisVector(0, 1))

		ctxA, err := tenant.WithTenant(ctx, tenantA)
		Expect(err).NotTo(HaveOccurred())
		reader := &pgEmbeddingsReader{db: idb.tenantConn}

		ids, values, err := reader.SimilarityMatrix(ctxA, tenantA, []string{src, tgt}, model)
		Expect(err).NotTo(HaveOccurred())
		Expect(ids).To(ConsistOf(s1, t1), "s1 appears once")
		Expect(similarityAt(ids, values, s1, t1)).To(BeNumerically("~", 60, 0.01), "s1 -> t1 from s1's current embedding")
		Expect(similarityAt(ids, values, s1, s1)).To(BeNumerically("~", 100, 0.01), "s1 with itself")
	})

	It("reads a catalog's name for its own tenant only, as the restricted role", func() {
		ctx := context.Background()
		idb := openIntegrationDB(ctx, 2)

		tenantA := fmt.Sprintf("names-a-%s", uuid.New().String())
		tenantB := fmt.Sprintf("names-b-%s", uuid.New().String())
		Expect(db.EnsureTenant(ctx, idb.suPool, tenantA, "names-a")).To(Succeed(), "ensure tenant A")
		Expect(db.EnsureTenant(ctx, idb.suPool, tenantB, "names-b")).To(Succeed(), "ensure tenant B")
		own, others := "cat-own-"+uuid.NewString(), "cat-others-"+uuid.NewString()
		seedEmbeddedCatalog(ctx, idb, tenantA, own, "CSF 2.0", "names-model", nil)
		seedEmbeddedCatalog(ctx, idb, tenantB, others, "Secret Framework", "names-model", nil)

		ctxA, err := tenant.WithTenant(ctx, tenantA)
		Expect(err).NotTo(HaveOccurred())
		reader := NewPGCatalogNameReader(idb.tenantConn)

		name, err := reader.CatalogName(ctxA, tenantA, own)
		Expect(err).NotTo(HaveOccurred())
		Expect(name).To(Equal("CSF 2.0"))

		_, err = reader.CatalogName(ctxA, tenantA, others)
		Expect(err).To(MatchError(ErrCatalogNotFound), "tenant A read tenant B's catalog name")
		Expect(err.Error()).NotTo(ContainSubstring("Secret Framework"))
	})
})
