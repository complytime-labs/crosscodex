-- Drop requires analyzer tables

DROP POLICY IF EXISTS tenant_isolation ON requires_consensus;
DROP TABLE IF EXISTS requires_consensus;

DROP POLICY IF EXISTS tenant_isolation ON requires_votes;
DROP TABLE IF EXISTS requires_votes;

DROP POLICY IF EXISTS tenant_isolation ON requires_candidates;
DROP TABLE IF EXISTS requires_candidates;
