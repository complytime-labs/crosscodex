-- Reverses 003_mapping_feedback.up.sql. Votes, determinations and reviews are
-- lost. Findings and decisions are kept, so restoring the original checks
-- fails while a 'stale' finding or decision exists: decisions are append-only
-- and cannot be removed, so recreate the database instead.

DROP VIEW IF EXISTS mapping_cleaning_states;

-- Dropping a table also drops its policy, grants, indexes and triggers.
DROP TABLE IF EXISTS mapping_reviews;
DROP TABLE IF EXISTS mapping_determinations;
DROP TABLE IF EXISTS mapping_votes;

ALTER TABLE edge_finding_decisions
    DROP CONSTRAINT IF EXISTS edge_finding_decisions_reproject,
    DROP CONSTRAINT edge_finding_decisions_verdict_check,
    ADD CONSTRAINT edge_finding_decisions_verdict_check
        CHECK (verdict IN ('retire', 'dismiss', 'restore'));

ALTER TABLE edge_findings
    DROP CONSTRAINT edge_findings_status_check,
    ADD CONSTRAINT edge_findings_status_check
        CHECK (status IN ('reported', 'pending', 'retired', 'dismissed', 'restored', 'cleared'));
