-- Teardown for initial schema. Reverse dependency order.

-- Graph lifecycle
DROP TRIGGER IF EXISTS tenant_graph_drop ON tenants;
DROP FUNCTION IF EXISTS public.drop_tenant_graph();

DROP TRIGGER IF EXISTS tenant_graph_create ON tenants;
DROP FUNCTION IF EXISTS public.create_tenant_graph();

DROP FUNCTION IF EXISTS public.assert_tenant_graph(TEXT);
DROP FUNCTION IF EXISTS public.tenant_graph_name();

DROP ROLE IF EXISTS graph_user;

-- Immutability triggers
DROP TRIGGER IF EXISTS classifications_immutable_delete ON classifications;
DROP TRIGGER IF EXISTS classifications_immutable_update ON classifications;
DROP FUNCTION IF EXISTS prevent_classification_mutation();

DROP TRIGGER IF EXISTS vote_summaries_immutable_delete ON vote_summaries;
DROP TRIGGER IF EXISTS vote_summaries_immutable_update ON vote_summaries;

DROP TRIGGER IF EXISTS job_stages_immutable_delete ON job_stages;
DROP TRIGGER IF EXISTS job_stages_immutable_update ON job_stages;

DROP FUNCTION IF EXISTS prevent_completed_job_child_delete();
DROP FUNCTION IF EXISTS prevent_completed_job_child_mutation();

DROP TRIGGER IF EXISTS jobs_immutable_delete ON jobs;
DROP TRIGGER IF EXISTS jobs_immutable_update ON jobs;

DROP FUNCTION IF EXISTS prevent_completed_job_delete();
DROP FUNCTION IF EXISTS prevent_completed_job_mutation();

-- RLS policies
DROP POLICY IF EXISTS tenant_isolation ON requires_consensus;
DROP POLICY IF EXISTS tenant_isolation ON requires_votes;
DROP POLICY IF EXISTS tenant_isolation ON requires_candidates;
DROP POLICY IF EXISTS tenant_isolation ON controls;
DROP POLICY IF EXISTS tenant_isolation ON embeddings;
DROP POLICY IF EXISTS tenant_isolation ON vote_summaries;
DROP POLICY IF EXISTS tenant_isolation ON classifications;
DROP POLICY IF EXISTS tenant_isolation ON catalogs;
DROP POLICY IF EXISTS tenant_isolation ON job_stages;
DROP POLICY IF EXISTS tenant_isolation ON jobs;
DROP POLICY IF EXISTS tenant_isolation ON tenants;

-- Revoke and drop app_user
REVOKE SELECT, INSERT, UPDATE, DELETE
    ON ALL TABLES IN SCHEMA public
    FROM app_user;

DROP ROLE IF EXISTS app_user;

-- Tables (reverse dependency order)
DROP TABLE IF EXISTS requires_consensus;
DROP TABLE IF EXISTS requires_votes;
DROP TABLE IF EXISTS requires_candidates;
DROP TABLE IF EXISTS controls;
DROP INDEX IF EXISTS idx_embeddings_vector;
DROP TABLE IF EXISTS embeddings;
DROP TABLE IF EXISTS vote_summaries;
DROP TABLE IF EXISTS classifications;
DROP TABLE IF EXISTS catalogs;
DROP TABLE IF EXISTS job_stages;
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS tenants;
