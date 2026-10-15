-- Reverses 009_identity_lifecycle.up.sql in reverse order. Restores the
-- hard-delete path: tenants and verdicts become deletable again and deleting
-- a tenant drops its graph.
--
-- Rolling back is refused while any tenant is deleted, because 007 has no
-- 'deleted' status, and while identity_events holds any row, because the
-- audit log is permanent like the judgements it records.

DO $$
DECLARE
    v_events bigint;
BEGIN
    IF EXISTS (SELECT 1 FROM public.tenants WHERE status = 'deleted') THEN
        RAISE EXCEPTION 'cannot roll back 009: some tenants are deleted. Restoring them with crosscodexd admin tenant restore is not enough: rollback is also refused while identity_events holds audit records, and every restore adds one. Export the audit records and remove them deliberately as the owner as well before rolling back';
    END IF;
    SELECT count(*) INTO v_events FROM public.identity_events;
    IF v_events > 0 THEN
        RAISE EXCEPTION 'cannot roll back 009: identity_events holds % audit records, which are never deleted. Export them and remove them deliberately as the owner before rolling back', v_events;
    END IF;
END $$;

-- Reviewer rows are dropped with the table. Every reviewer change wrote an
-- identity_events row, which refuses the rollback above, so only reviewers
-- registered by the 009 backfill can be lost, and re-applying 009 backfills
-- them again.
DROP FUNCTION public.require_active_reviewer(text);
DROP FUNCTION public.trace_pseudonym(text, text, text, bytea, bytea);
DROP FUNCTION public.list_reviewers(text);
DROP FUNCTION public.unhide_reviewer(text, text, text, text, bytea, bytea);
DROP FUNCTION public.hide_reviewer(text, text, text, text, bytea, bytea);
DROP FUNCTION public.restore_reviewer(text, text, text, text, bytea, bytea);
DROP FUNCTION public.delete_reviewer(text, text, text, text, bytea, bytea);
DROP FUNCTION public.add_reviewer(text, text, text, text, bytea, bytea);
DROP TABLE public.reviewers;

DROP FUNCTION public.enter_tenant(text);
DROP FUNCTION public.unhide_tenant(text, text, text, bytea, bytea);
DROP FUNCTION public.hide_tenant(text, text, text, bytea, bytea);
DROP FUNCTION public.restore_tenant(text, text, text, bytea, bytea);
DROP FUNCTION public.delete_tenant(text, text, text, bytea, bytea);

-- list_tenants, set_tenant_status and provision_tenant exactly as in 007.
-- CREATE OR REPLACE keeps the grants of the latter two.
DROP FUNCTION public.list_tenants();
CREATE FUNCTION public.list_tenants()
RETURNS TABLE (tenant_id text, display_name text, status text, created_at timestamptz)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT t.tenant_id, t.display_name, t.status, t.created_at
    FROM public.tenants t
    ORDER BY t.tenant_id
$$;
REVOKE ALL ON FUNCTION public.list_tenants() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.list_tenants() TO tenant_admin;

CREATE OR REPLACE FUNCTION public.set_tenant_status(p_id text, p_status text)
RETURNS text
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_previous text;
BEGIN
    IF p_status IS NULL OR p_status NOT IN ('active', 'suspended') THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = format(
            'tenant status %L is invalid: it must be ''active'' or ''suspended''', left(p_status, 64));
    END IF;

    SELECT status INTO v_previous FROM public.tenants WHERE tenant_id = p_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = format(
            'tenant %L does not exist; nothing was changed. Create it with ''crosscodexd admin tenant create --tenant <id> --display-name <name>''',
            left(p_id, 64));
    END IF;

    UPDATE public.tenants SET status = p_status WHERE tenant_id = p_id;
    RETURN v_previous;
END
$$;

CREATE OR REPLACE FUNCTION public.provision_tenant(p_id text, p_name text)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF p_id IS NULL OR p_id !~ '^[a-z][a-z0-9-]{1,50}[a-z0-9]$' THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = format(
            'tenant ID %L is invalid: it must be 3-52 characters, lowercase letters, digits and hyphens, starting with a letter and not ending with a hyphen. Choose an ID that matches',
            left(p_id, 64));
    END IF;
    IF p_name IS NULL OR p_name ~ '^\s*$' THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = format(
            'tenant %L: display name is empty; give the tenant a human-readable name', left(p_id, 64));
    END IF;
    IF p_name ~ '[\u0001-\u001F\u007F-\u009F]' THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = format(
            'tenant %L: display name contains a control character; use printable text only', left(p_id, 64));
    END IF;

    INSERT INTO public.tenants (tenant_id, display_name) VALUES (p_id, p_name)
        ON CONFLICT (tenant_id) DO NOTHING;
    IF FOUND THEN
        RETURN true;
    END IF;

    UPDATE public.tenants SET display_name = p_name WHERE tenant_id = p_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = '40001', MESSAGE = format(
            'tenant %L was deleted while it was being provisioned; nothing was changed. Run the command again', left(p_id, 64));
    END IF;
    RETURN false;
END
$$;

DROP TABLE public.identity_events;
DROP FUNCTION public.record_identity_event(text, text, text, text, text, text, text, bytea, bytea);

ALTER TABLE public.tenants
    DROP CONSTRAINT tenants_id_not_reserved,
    DROP CONSTRAINT tenants_pseudonym_format,
    DROP CONSTRAINT tenants_hidden_consistent,
    DROP CONSTRAINT tenants_deleted_consistent,
    DROP CONSTRAINT tenants_status_check,
    ADD CONSTRAINT tenants_status_check CHECK (status IN ('active', 'suspended'));
ALTER TABLE public.tenants
    DROP COLUMN hidden_at,
    DROP COLUMN pseudonym,
    DROP COLUMN deleted_by,
    DROP COLUMN deleted_at;

DROP TRIGGER tenants_refuse_truncate ON public.tenants;
DROP TRIGGER tenants_refuse_delete ON public.tenants;
DROP TRIGGER artifact_pair_verdicts_refuse_truncate ON public.artifact_pair_verdicts;
DROP TRIGGER artifact_pair_verdicts_refuse_delete ON public.artifact_pair_verdicts;

CREATE OR REPLACE FUNCTION public.drop_tenant_graph()
RETURNS TRIGGER AS $$
BEGIN
    PERFORM ag_catalog.drop_graph('crosscodex_' || OLD.tenant_id, true);
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER tenant_graph_drop
    BEFORE DELETE ON tenants
    FOR EACH ROW EXECUTE FUNCTION public.drop_tenant_graph();

GRANT DELETE, UPDATE ON public.tenants TO app_user;

ALTER TABLE public.artifact_pair_verdicts
    DROP CONSTRAINT artifact_pair_verdicts_tenant_id_fkey,
    ADD CONSTRAINT artifact_pair_verdicts_tenant_id_fkey
        FOREIGN KEY (tenant_id) REFERENCES public.tenants (tenant_id);

DROP FUNCTION public.refuse_change();
