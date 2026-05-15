-- Immutability triggers for completed job data.
-- Error messages are actionable: they tell the operator what happened,
-- why it was blocked, and what to do instead.

CREATE OR REPLACE FUNCTION prevent_completed_job_mutation()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.status = 'completed' THEN
        RAISE EXCEPTION 'cannot modify job %: status is "completed". To retry, create a new job instead of resetting this one.',
            OLD.job_id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION prevent_completed_job_delete()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.status = 'completed' THEN
        RAISE EXCEPTION 'cannot delete job %: status is "completed". Completed jobs are retained for audit. See retention policy (ticket #34).',
            OLD.job_id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER jobs_immutable_update
    BEFORE UPDATE ON jobs
    FOR EACH ROW EXECUTE FUNCTION prevent_completed_job_mutation();

CREATE TRIGGER jobs_immutable_delete
    BEFORE DELETE ON jobs
    FOR EACH ROW EXECUTE FUNCTION prevent_completed_job_delete();

CREATE OR REPLACE FUNCTION prevent_completed_job_child_mutation()
RETURNS TRIGGER AS $$
DECLARE
    parent_status TEXT;
BEGIN
    SELECT status INTO parent_status FROM jobs WHERE job_id = OLD.job_id;
    IF parent_status = 'completed' THEN
        RAISE EXCEPTION 'cannot modify % record: parent job % is "completed". Completed job data is immutable.',
            TG_TABLE_NAME, OLD.job_id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION prevent_completed_job_child_delete()
RETURNS TRIGGER AS $$
DECLARE
    parent_status TEXT;
BEGIN
    SELECT status INTO parent_status FROM jobs WHERE job_id = OLD.job_id;
    IF parent_status = 'completed' THEN
        RAISE EXCEPTION 'cannot delete % record: parent job % is "completed". Completed job data is immutable.',
            TG_TABLE_NAME, OLD.job_id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER job_stages_immutable_update
    BEFORE UPDATE ON job_stages
    FOR EACH ROW EXECUTE FUNCTION prevent_completed_job_child_mutation();

CREATE TRIGGER job_stages_immutable_delete
    BEFORE DELETE ON job_stages
    FOR EACH ROW EXECUTE FUNCTION prevent_completed_job_child_delete();

CREATE TRIGGER vote_summaries_immutable_update
    BEFORE UPDATE ON vote_summaries
    FOR EACH ROW EXECUTE FUNCTION prevent_completed_job_child_mutation();

CREATE TRIGGER vote_summaries_immutable_delete
    BEFORE DELETE ON vote_summaries
    FOR EACH ROW EXECUTE FUNCTION prevent_completed_job_child_delete();

CREATE OR REPLACE FUNCTION prevent_classification_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'cannot modify classification (catalog_id=%, control_id=%, type=%): classifications are write-once. Insert a new version instead.',
        OLD.catalog_id, OLD.control_id, OLD.type
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER classifications_immutable_update
    BEFORE UPDATE ON classifications
    FOR EACH ROW EXECUTE FUNCTION prevent_classification_mutation();

CREATE TRIGGER classifications_immutable_delete
    BEFORE DELETE ON classifications
    FOR EACH ROW EXECUTE FUNCTION prevent_classification_mutation();
