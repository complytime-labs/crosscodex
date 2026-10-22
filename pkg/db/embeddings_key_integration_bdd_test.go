//go:build integration

package db_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// embeddings is keyed by tenant, so one tenant's upsert can never collide
// with another tenant's row.
var _ = Describe("embeddings tenant key", func() {
	It("leads the primary key with tenant_id", func() {
		var cols string
		Expect(suPool.QueryRow(context.Background(), `
			SELECT string_agg(a.attname, ',' ORDER BY k.ord)
			FROM pg_index i
			CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
			JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
			WHERE i.indrelid = 'public.embeddings'::regclass AND i.indisprimary`).Scan(&cols)).To(Succeed())
		Expect(cols).To(Equal("tenant_id,catalog_id,control_id,model"))
	})
})
