-- artifact_pair_verdicts is the work queue and verdict record for SAME_AS
-- candidates between ArtifactGroup nodes (issue #170). The reconciler
-- enqueues token-overlap matches; `crosscodexd admin adjudicate artifacts`
-- leases pending rows, runs an LLM panel and records the verdict. The graph
-- is a projection of this table, never the other way round.
--
-- determination_type ranks authority: human > llm_panel > token_overlap.
-- Automation never leases, overwrites or projects a human row.
--
-- low_group_id < high_group_id is compared under COLLATE "C" so the check
-- matches Go's byte-wise string order whatever the database collation.
CREATE TABLE IF NOT EXISTS artifact_pair_verdicts (
    tenant_id          TEXT NOT NULL REFERENCES tenants(tenant_id),
    pair_key           TEXT NOT NULL,
    low_group_id       TEXT NOT NULL,
    high_group_id      TEXT NOT NULL,
    candidate_edge_id  TEXT NOT NULL,
    similarity_score   DOUBLE PRECISION NOT NULL,
    context            JSONB NOT NULL,
    status             TEXT NOT NULL DEFAULT 'pending',
    determination_type TEXT NOT NULL DEFAULT 'token_overlap',
    determined_by      TEXT NOT NULL,
    confidence         DOUBLE PRECISION,
    evidence           JSONB,
    dissent            JSONB,
    graph_applied      BOOLEAN NOT NULL DEFAULT false,
    attempts           INT NOT NULL DEFAULT 0,
    next_attempt_at    TIMESTAMPTZ NOT NULL,
    leased_until       TIMESTAMPTZ,
    last_error         TEXT,
    created_at         TIMESTAMPTZ NOT NULL,
    decided_at         TIMESTAMPTZ,
    PRIMARY KEY (tenant_id, pair_key),
    CONSTRAINT artifact_pair_verdicts_order
        CHECK (low_group_id COLLATE "C" < high_group_id COLLATE "C"),
    CONSTRAINT artifact_pair_verdicts_status
        CHECK (status IN ('pending', 'confirmed', 'rejected', 'undecided', 'abandoned')),
    CONSTRAINT artifact_pair_verdicts_determination
        CHECK (determination_type IN ('token_overlap', 'llm_panel', 'human')),
    CONSTRAINT artifact_pair_verdicts_context_object
        CHECK (jsonb_typeof(context) = 'object'),
    CONSTRAINT artifact_pair_verdicts_evidence_object
        CHECK (evidence IS NULL OR jsonb_typeof(evidence) = 'object'),
    CONSTRAINT artifact_pair_verdicts_dissent_object
        CHECK (dissent IS NULL OR jsonb_typeof(dissent) = 'object')
);

-- Serves the adjudicator's lease query and, through its (tenant_id, status)
-- prefix, the reconciler's closed-pair load and the dry-run counts.
CREATE INDEX IF NOT EXISTS idx_artifact_pair_verdicts_queue
    ON artifact_pair_verdicts (tenant_id, status, next_attempt_at);

ALTER TABLE artifact_pair_verdicts ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON artifact_pair_verdicts
    USING (tenant_id = current_setting('app.current_tenant', true));

-- No DELETE: verdicts are a permanent record; nothing in the app removes one.
GRANT SELECT, INSERT, UPDATE ON artifact_pair_verdicts TO app_user;
