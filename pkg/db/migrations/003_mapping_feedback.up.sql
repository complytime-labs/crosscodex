-- Mapping feedback, determinations and the cleaning state view
-- (issues #230, #291, #289). See docs/dev/feedback-cleaning-results-spec.md.

-- ============================================================================
-- Reprojection: the 'stale' finding status and decision verdict
-- ============================================================================
-- A cleaning reprojection (crosscodexd admin ... --reproject) marks findings
-- whose verdicts no longer exist in the graph as 'stale' rather than deleting
-- them. Only the reprojection records that verdict, as decided_by 'reproject',
-- and it records nothing else, so the two are tied by a CHECK.
-- The status and verdict CHECKs are inline in 001, so PostgreSQL named them
-- <table>_<column>_check.
ALTER TABLE edge_findings
    DROP CONSTRAINT edge_findings_status_check,
    ADD CONSTRAINT edge_findings_status_check
        CHECK (status IN ('reported', 'pending', 'retired', 'dismissed', 'restored', 'cleared', 'stale'));

ALTER TABLE edge_finding_decisions
    DROP CONSTRAINT edge_finding_decisions_verdict_check,
    ADD CONSTRAINT edge_finding_decisions_verdict_check
        CHECK (verdict IN ('retire', 'dismiss', 'restore', 'stale')),
    ADD CONSTRAINT edge_finding_decisions_reproject
        CHECK ((verdict = 'stale') = (decided_by = 'reproject'));

-- ============================================================================
-- Mapping votes, determinations and reviews
-- ============================================================================
-- mapping_votes is the append-only record of feedback on a job's mapping
-- (source_id, target_id in job_id): who voted, how, and the evidence they cite.
-- suggested_type is the relationship a 'refine' vote proposes and only a
-- 'refine' vote carries one.
--
-- mapping_determinations is the append-only record of every valid / invalid /
-- stale call on a mapping: decided_by is a reviewer ID or 'reproject' (outcome
-- 'stale'); request is the canonical request and attestation its signed
-- in-toto link, both stored byte for byte.
--
-- mapping_reviews holds the current determination of each mapping, so it is
-- upserted: determination_id names the determination that set it, edge_ids
-- the graph edges the determination changed and graph_applied is false until
-- those edge changes are written. The graph is a projection of these rows.
--
-- app_user is a restricted role so that a compromised daemon cannot rewrite
-- who voted or decided what: it may only read and append votes and
-- determinations, and read, insert and update reviews (never delete them).
-- Rows are never deleted because the record is the audit trail; to reverse a
-- determination record a new one. The refusal triggers hold for every role,
-- the owner included.
CREATE TABLE IF NOT EXISTS mapping_votes (
    tenant_id      TEXT NOT NULL REFERENCES tenants(tenant_id) ON DELETE RESTRICT,
    vote_id        TEXT NOT NULL,
    mapping_id     TEXT NOT NULL,
    job_id         TEXT NOT NULL,
    source_id      TEXT NOT NULL,
    target_id      TEXT NOT NULL,
    voter          TEXT NOT NULL,
    vote_type      TEXT NOT NULL CHECK (vote_type IN ('agree', 'disagree', 'refine', 'escalate')),
    suggested_type TEXT,
    rationale      TEXT NOT NULL,
    evidence       JSONB NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, vote_id),
    -- Only a refine vote proposes a relationship type, and it must.
    CONSTRAINT mapping_votes_suggested_type CHECK ((vote_type = 'refine') = (suggested_type IS NOT NULL))
);

-- Serves the votes of one mapping in a job, per voter, oldest first.
CREATE INDEX IF NOT EXISTS idx_mapping_votes_mapping
    ON mapping_votes (tenant_id, job_id, mapping_id, voter, created_at);

ALTER TABLE mapping_votes ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON mapping_votes
    USING (tenant_id = current_setting('app.current_tenant', true));

-- SELECT and INSERT only: votes are never changed or removed.
GRANT SELECT, INSERT ON mapping_votes TO app_user;

CREATE TRIGGER mapping_votes_refuse_update
    BEFORE UPDATE OR DELETE ON public.mapping_votes
    FOR EACH ROW EXECUTE FUNCTION public.refuse_change(
        'mapping_votes is an append-only record of who voted what; rows are never changed or removed. To change your view of a mapping record a new vote.');
CREATE TRIGGER mapping_votes_refuse_truncate
    BEFORE TRUNCATE ON public.mapping_votes
    FOR EACH STATEMENT EXECUTE FUNCTION public.refuse_change(
        'mapping_votes is an append-only record of who voted what, so TRUNCATE is refused. To change your view of a mapping record a new vote.');

CREATE TABLE IF NOT EXISTS mapping_determinations (
    tenant_id        TEXT NOT NULL REFERENCES tenants(tenant_id) ON DELETE RESTRICT,
    determination_id TEXT NOT NULL,
    mapping_id       TEXT NOT NULL,
    job_id           TEXT NOT NULL,
    source_id        TEXT NOT NULL,
    target_id        TEXT NOT NULL,
    outcome          TEXT NOT NULL CHECK (outcome IN ('valid', 'invalid', 'stale')),
    decided_by       TEXT NOT NULL,
    rationale        TEXT NOT NULL,
    request          BYTEA NOT NULL,
    attestation      BYTEA NOT NULL,
    decided_at       TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, determination_id),
    -- Only the reprojection marks a mapping stale, and it records nothing else.
    CONSTRAINT mapping_determinations_reproject CHECK ((outcome = 'stale') = (decided_by = 'reproject'))
);

-- Serves a mapping's determination history and the reviews foreign key's checks.
CREATE INDEX IF NOT EXISTS idx_mapping_determinations_mapping
    ON mapping_determinations (tenant_id, mapping_id, decided_at);

ALTER TABLE mapping_determinations ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON mapping_determinations
    USING (tenant_id = current_setting('app.current_tenant', true));

-- SELECT and INSERT only: determinations are never changed or removed.
GRANT SELECT, INSERT ON mapping_determinations TO app_user;

CREATE TRIGGER mapping_determinations_refuse_update
    BEFORE UPDATE OR DELETE ON public.mapping_determinations
    FOR EACH ROW EXECUTE FUNCTION public.refuse_change(
        'mapping_determinations is an append-only record of who decided what; rows are never changed or removed. To reverse a determination record a new one.');
CREATE TRIGGER mapping_determinations_refuse_truncate
    BEFORE TRUNCATE ON public.mapping_determinations
    FOR EACH STATEMENT EXECUTE FUNCTION public.refuse_change(
        'mapping_determinations is an append-only record of who decided what, so TRUNCATE is refused. To reverse a determination record a new one.');

CREATE TABLE IF NOT EXISTS mapping_reviews (
    tenant_id        TEXT NOT NULL REFERENCES tenants(tenant_id) ON DELETE RESTRICT,
    mapping_id       TEXT NOT NULL,
    job_id           TEXT NOT NULL,
    source_id        TEXT NOT NULL,
    target_id        TEXT NOT NULL,
    status           TEXT NOT NULL CHECK (status IN ('valid', 'invalid', 'stale')),
    determination_id TEXT NOT NULL,
    decided_at       TIMESTAMPTZ NOT NULL,
    edge_ids         JSONB NOT NULL,
    graph_applied    BOOLEAN NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, mapping_id),
    FOREIGN KEY (tenant_id, determination_id) REFERENCES mapping_determinations (tenant_id, determination_id)
);

ALTER TABLE mapping_reviews ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON mapping_reviews
    USING (tenant_id = current_setting('app.current_tenant', true));

-- No DELETE: a reversed determination replaces the row's status.
GRANT SELECT, INSERT, UPDATE ON mapping_reviews TO app_user;

-- ============================================================================
-- Cleaning state view
-- ============================================================================
-- mapping_cleaning_states is the one rule for what cleaning and reviewers have
-- done to a mapping (#230, #291): the verdicts a cleaning finding retired,
-- restored or marked stale (a finding's targets are JSON objects with job_id, source_id and
-- target_id, cleaning.VerdictKey),
-- and the mapping reviews that retired, marked stale or restored one
-- (a 'valid' review counts only when it changed edges, edge_ids not empty).
-- A mapping can appear several times; consumers pick the latest row by
-- (changed_at DESC, source, ref_id). security_invoker makes the view run with
-- the caller's row-level security, so tenant isolation holds through it.
CREATE VIEW mapping_cleaning_states WITH (security_invoker = true) AS
    SELECT f.tenant_id,
           t.key ->> 'job_id'        AS job_id,
           t.key ->> 'source_id'     AS source_id,
           t.key ->> 'target_id'     AS target_id,
           f.status                  AS state,
           'finding'                 AS source,
           f.finding_id              AS ref_id,
           f.updated_at              AS changed_at,
           f.graph_applied           AS graph_applied
    FROM edge_findings f
    CROSS JOIN LATERAL jsonb_array_elements(f.targets) AS t(key)
    WHERE f.status IN ('retired', 'restored', 'stale')
    UNION ALL
    SELECT r.tenant_id,
           r.job_id,
           r.source_id,
           r.target_id,
           CASE r.status WHEN 'invalid' THEN 'retired'
                         WHEN 'stale'   THEN 'stale'
                         ELSE 'restored' END AS state,
           'determination'                   AS source,
           r.determination_id                AS ref_id,
           r.decided_at                      AS changed_at,
           r.graph_applied                   AS graph_applied
    FROM mapping_reviews r
    WHERE r.status IN ('invalid', 'stale')
       OR (r.status = 'valid' AND r.edge_ids <> '[]'::jsonb);

GRANT SELECT ON mapping_cleaning_states TO app_user;
