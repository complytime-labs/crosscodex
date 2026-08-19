CREATE TABLE IF NOT EXISTS analysis_results (
    tenant_id     TEXT        NOT NULL REFERENCES tenants(tenant_id),
    job_id        TEXT        NOT NULL REFERENCES jobs(job_id),
    analyzer_name TEXT        NOT NULL,
    result_data   JSONB       NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, job_id, analyzer_name)
);

ALTER TABLE analysis_results ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON analysis_results
    USING (tenant_id = current_setting('app.current_tenant', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON analysis_results TO app_user;

CREATE TABLE IF NOT EXISTS relationship_candidates (
    tenant_id        TEXT        NOT NULL REFERENCES tenants(tenant_id),
    job_id           TEXT        NOT NULL REFERENCES jobs(job_id),
    source_id        TEXT        NOT NULL,
    target_id        TEXT        NOT NULL,
    similarity_score REAL        NOT NULL,             -- [0, 100] from embedding similarity matrix (see relationship.CandidatePair.SimilarityScore) -- NOTE: differs in scale from requires_candidates.aggregate_score, which is [0.0, 1.0]
    provenance       JSONB       NOT NULL DEFAULT '[]',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, job_id, source_id, target_id)
);

CREATE INDEX IF NOT EXISTS idx_relationship_candidates_job
    ON relationship_candidates (tenant_id, job_id);

ALTER TABLE relationship_candidates ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON relationship_candidates
    USING (tenant_id = current_setting('app.current_tenant', true));

GRANT SELECT, INSERT, UPDATE, DELETE ON relationship_candidates TO app_user;
