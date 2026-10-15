-- Migration 009 (#195): judgements are never deleted; tenants and reviewers
-- are soft-deleted, can be hidden behind a pseudonym an admin can trace, and
-- every identity change is recorded in identity_events with a signed in-toto
-- link (docs/dev/design-principles.md "Identity lifecycle").
--
-- Why the roles are restricted: app_user and graph_user serve network
-- requests, so a compromised or buggy service must not be able to erase or
-- rewrite who decided what. app_user loses DELETE and UPDATE on tenants (no
-- app code needs them; UPDATE would let a tenant-scoped session un-delete its
-- own tenant) and gets SELECT only on reviewers. tenant_admin keeps no table
-- grant and acts only through the SECURITY DEFINER functions below.
-- identity_events has no grant to any role.
--
-- Refusal triggers fire for every role, including the table owner and
-- superusers. They can be bypassed only by a role able to disable them: the
-- table owner (ALTER TABLE ... DISABLE TRIGGER) or a superuser (that, or
-- session_replication_role = replica). This relies on migrations running as
-- the owner and on no application role (app_user, graph_user, tenant_admin,
-- backup_user, the retention roles) owning these tables or being superuser.

-- Tenant IDs starting with 'hidden-' are reserved for pseudonyms of hidden
-- tenants (tenants_id_not_reserved below). Checked first, so a database
-- holding such an ID is refused with an actionable message before anything
-- changes, rather than by a bare constraint violation.
DO $$
DECLARE
    v_ids text;
BEGIN
    SELECT string_agg(quote_literal(left(tenant_id, 64)), ', ' ORDER BY tenant_id) INTO v_ids
    FROM public.tenants WHERE tenant_id LIKE 'hidden-%';
    IF v_ids IS NOT NULL THEN
        RAISE EXCEPTION USING ERRCODE = '23514', MESSAGE = format(
            'cannot apply 009: tenant IDs starting with ''hidden-'' are reserved for pseudonyms of hidden tenants, and these tenants use one: %s. Nothing was changed. Rename or remove them as the owner before migrating',
            v_ids);
    END IF;
END $$;

-- Every reviewer of an existing human verdict is registered in reviewers
-- below, so each one must satisfy the reviewers ID constraints
-- (reviewers_id_format, which mirrors artifacts.ValidateReviewer, and
-- reviewers_id_not_reserved). Checked first for the same reason as tenants.
-- The message lists at most 20 IDs, each cut to 64 characters with control
-- characters shown as \uXXXX, so it stays bounded and printable.
DO $$
DECLARE
    v_count bigint;
    v_ids text;
BEGIN
    WITH bad AS (
        SELECT DISTINCT v.tenant_id, v.determined_by
        FROM public.artifact_pair_verdicts v
        WHERE v.determination_type = 'human'
          AND (v.determined_by LIKE 'hidden-%'
               OR v.determined_by ~ '^\s*$' OR octet_length(v.determined_by) > 256
               OR v.determined_by ~ '[\u0001-\u001F\u007F-\u009F]' OR v.determined_by <> btrim(v.determined_by))
    ), shown AS (
        SELECT b.tenant_id, b.determined_by,
               (SELECT coalesce(string_agg(CASE WHEN ch.c ~ '[\u0001-\u001F\u007F-\u009F]'
                                                THEN '\u' || lpad(to_hex(ascii(ch.c)), 4, '0')
                                                WHEN ch.c = '''' THEN ''''''
                                                ELSE ch.c END, '' ORDER BY ch.n), '')
                FROM unnest(string_to_array(left(b.determined_by, 64), NULL)) WITH ORDINALITY AS ch(c, n)) AS escaped
        FROM bad b
        ORDER BY b.tenant_id, b.determined_by
        LIMIT 20
    )
    SELECT (SELECT count(*) FROM bad),
           (SELECT string_agg(format('''%s'' of tenant %L', s.escaped, s.tenant_id), ', '
                              ORDER BY s.tenant_id, s.determined_by) FROM shown s)
    INTO v_count, v_ids;
    IF v_count > 0 THEN
        RAISE EXCEPTION USING ERRCODE = '23514', MESSAGE = format(
            'cannot apply 009: every reviewer of a human verdict is registered, and these reviewer IDs cannot be: %s%s. Reviewer IDs must be 1-256 bytes of printable text without leading or trailing spaces, and IDs starting with ''hidden-'' are reserved for pseudonyms of hidden reviewers. Nothing was changed. Correct determined_by on those verdicts as the owner before migrating',
            v_ids, CASE WHEN v_count > 20 THEN format(' and %s more', v_count - 20) ELSE '' END);
    END IF;
END $$;

CREATE FUNCTION public.refuse_change() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    RAISE EXCEPTION USING ERRCODE = '42501', MESSAGE = TG_ARGV[0];
END
$$;
REVOKE ALL ON FUNCTION public.refuse_change() FROM PUBLIC;

ALTER TABLE public.artifact_pair_verdicts
    DROP CONSTRAINT artifact_pair_verdicts_tenant_id_fkey,
    ADD CONSTRAINT artifact_pair_verdicts_tenant_id_fkey
        FOREIGN KEY (tenant_id) REFERENCES public.tenants (tenant_id) ON DELETE RESTRICT;

CREATE TRIGGER artifact_pair_verdicts_refuse_delete
    BEFORE DELETE ON public.artifact_pair_verdicts
    FOR EACH ROW EXECUTE FUNCTION public.refuse_change(
        'artifact_pair_verdicts rows are permanent judgements and are never deleted. To retire a tenant run ''crosscodexd admin tenant delete'' (soft delete); to retire a reviewer run ''crosscodexd admin reviewer delete''.');
CREATE TRIGGER artifact_pair_verdicts_refuse_truncate
    BEFORE TRUNCATE ON public.artifact_pair_verdicts
    FOR EACH STATEMENT EXECUTE FUNCTION public.refuse_change(
        'artifact_pair_verdicts rows are permanent judgements and are never deleted, so TRUNCATE is refused. To retire a tenant run ''crosscodexd admin tenant delete'' (soft delete); to retire a reviewer run ''crosscodexd admin reviewer delete''.');

REVOKE DELETE, UPDATE ON public.tenants FROM app_user;

-- Tenants are never hard-deleted, so the trigger that dropped a deleted
-- tenant's graph can no longer fire legitimately.
DROP TRIGGER tenant_graph_drop ON public.tenants;
DROP FUNCTION public.drop_tenant_graph();

CREATE TRIGGER tenants_refuse_delete
    BEFORE DELETE ON public.tenants
    FOR EACH ROW EXECUTE FUNCTION public.refuse_change(
        'tenants are never hard-deleted because their judgements are permanent. Run ''crosscodexd admin tenant delete --tenant <id> --by <operator> --reason <text>'' to soft-delete instead.');
CREATE TRIGGER tenants_refuse_truncate
    BEFORE TRUNCATE ON public.tenants
    FOR EACH STATEMENT EXECUTE FUNCTION public.refuse_change(
        'tenants are never hard-deleted because their judgements are permanent, so TRUNCATE is refused. Run ''crosscodexd admin tenant delete --tenant <id> --by <operator> --reason <text>'' to soft-delete each tenant instead.');

-- Tenant lifecycle. pkg/db TenantStatus* mirror the status list. A deleted
-- tenant keeps its row and data; it may additionally be hidden, after which
-- list_tenants shows only its pseudonym. The pseudonym is kept on unhide and
-- restore so hiding again yields the same one and old reports stay traceable.
ALTER TABLE public.tenants
    ADD COLUMN deleted_at timestamptz,
    ADD COLUMN deleted_by text,
    ADD COLUMN pseudonym  text UNIQUE,
    ADD COLUMN hidden_at  timestamptz;
ALTER TABLE public.tenants
    DROP CONSTRAINT tenants_status_check,
    ADD CONSTRAINT tenants_status_check CHECK (status IN ('active', 'suspended', 'deleted')),
    ADD CONSTRAINT tenants_deleted_consistent
        CHECK ((status = 'deleted') = (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)),
    ADD CONSTRAINT tenants_hidden_consistent
        CHECK (hidden_at IS NULL OR (status = 'deleted' AND pseudonym IS NOT NULL)),
    ADD CONSTRAINT tenants_pseudonym_format
        CHECK (pseudonym IS NULL OR pseudonym ~ '^hidden-tenant-[0-9a-f]{16}$'),
    -- A constraint, not only provision_tenant's check, because EnsureTenant
    -- inserts tenants directly.
    ADD CONSTRAINT tenants_id_not_reserved CHECK (tenant_id NOT LIKE 'hidden-%');

-- identity_events is the append-only audit log of identity changes and
-- traces. request holds the canonical request JSON; attestation is a signed
-- in-toto link whose single material digest is sha256(request), so a
-- verifier needs only the row and the public key. No role is granted any
-- privilege on it: rows are written only by the SECURITY DEFINER functions
-- below and read by the owner.
CREATE TABLE public.identity_events (
    event_id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    occurred_at        timestamptz NOT NULL DEFAULT now(),
    action             text NOT NULL CHECK (action IN ('add', 'delete', 'restore', 'hide', 'unhide', 'trace')),
    kind               text NOT NULL CHECK (kind IN ('tenant', 'reviewer')),
    tenant_id          text NOT NULL REFERENCES public.tenants (tenant_id) ON DELETE RESTRICT,
    identity_id        text NOT NULL,
    actor              text NOT NULL,
    db_user            text NOT NULL DEFAULT session_user,
    reason             text NOT NULL,
    request            bytea NOT NULL,
    attestation        bytea NOT NULL,
    attestation_sha256 text GENERATED ALWAYS AS (encode(sha256(attestation), 'hex')) STORED
);
REVOKE ALL ON public.identity_events FROM PUBLIC;
CREATE TRIGGER identity_events_refuse_update
    BEFORE UPDATE OR DELETE ON public.identity_events
    FOR EACH ROW EXECUTE FUNCTION public.refuse_change(
        'identity_events is an append-only audit log; rows are never changed or removed.');
CREATE TRIGGER identity_events_refuse_truncate
    BEFORE TRUNCATE ON public.identity_events
    FOR EACH STATEMENT EXECUTE FUNCTION public.refuse_change(
        'identity_events is an append-only audit log; rows are never changed or removed.');

-- record_identity_event validates the operator input every action takes and
-- appends one event. It is not SECURITY DEFINER and no role may EXECUTE it:
-- only the definer functions below, running as the owner, call it.
--
-- The signed request must name this exact change, so a request signed for
-- one tenant or identity cannot be replayed against another. p_subject is
-- the identity the request names: p_identity for every action except
-- 'trace', whose request names the pseudonym being traced and, because the
-- operator does not know them yet, leaves kind and tenant_id empty.
CREATE FUNCTION public.record_identity_event(
    p_action text, p_kind text, p_tenant text, p_identity text, p_subject text,
    p_actor text, p_reason text, p_request bytea, p_attestation bytea)
RETURNS void
LANGUAGE plpgsql
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_req      jsonb;
    v_expected jsonb;
    v_differs  text;
BEGIN
    IF p_actor IS NULL OR p_actor ~ '^\s*$' OR octet_length(p_actor) > 256
       OR p_actor ~ '[\u0001-\u001F\u007F-\u009F]' OR p_actor <> btrim(p_actor) THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE =
            'actor (--by) must be 1-256 bytes of printable text without leading or trailing spaces; nothing was changed';
    END IF;
    IF p_reason IS NULL OR p_reason ~ '^\s*$' OR octet_length(p_reason) > 500
       OR p_reason ~ '[\u0001-\u001F\u007F-\u009F]' THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE =
            'reason (--reason) must be 1-500 bytes of printable text; nothing was changed';
    END IF;
    IF p_attestation IS NULL OR octet_length(p_attestation) = 0 THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE =
            'identity change has no attestation; nothing was changed. Run it through crosscodexd admin, which signs every change';
    END IF;
    IF octet_length(p_request) > 4096 THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE =
            'identity request is larger than 4096 bytes; nothing was changed. Run it through crosscodexd admin, which builds a request of bounded size';
    END IF;
    IF octet_length(p_attestation) > 65536 THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE =
            'identity attestation is larger than 65536 bytes; nothing was changed. Run it through crosscodexd admin, which signs a single link';
    END IF;
    BEGIN
        v_req := convert_from(p_request, 'UTF8')::jsonb;
    EXCEPTION WHEN OTHERS THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE =
            'identity request is not valid UTF-8 JSON; nothing was changed. Run it through crosscodexd admin';
    END;
    v_expected := jsonb_build_object('action', p_action, 'actor', p_actor, 'reason', p_reason, 'identity_id', p_subject);
    IF p_action <> 'trace' THEN
        v_expected := v_expected || jsonb_build_object('kind', p_kind, 'tenant_id', p_tenant);
    END IF;
    SELECT string_agg(e.key, ', ' ORDER BY e.key) INTO v_differs
    FROM jsonb_each_text(v_expected) e
    WHERE v_req->>e.key IS DISTINCT FROM e.value;
    IF v_differs IS NOT NULL THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = format(
            'signed identity request does not match the change: its %s %s; nothing was changed. Sign a new request for this change',
            v_differs, CASE WHEN strpos(v_differs, ',') > 0 THEN 'differ' ELSE 'differs' END);
    END IF;
    INSERT INTO public.identity_events
        (action, kind, tenant_id, identity_id, actor, reason, request, attestation)
    VALUES (p_action, p_kind, p_tenant, p_identity, p_actor, p_reason, p_request, p_attestation);
END
$$;
REVOKE ALL ON FUNCTION public.record_identity_event(text, text, text, text, text, text, text, bytea, bytea) FROM PUBLIC;

-- delete_tenant soft-deletes a tenant: its row, data and judgements stay,
-- and enter_tenant refuses it on every app path.
CREATE FUNCTION public.delete_tenant(p_id text, p_actor text, p_reason text, p_request bytea, p_attestation bytea)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_status text;
BEGIN
    SELECT status INTO v_status FROM public.tenants WHERE tenant_id = p_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = format(
            'tenant %L does not exist; nothing was changed. Check the ID with ''crosscodexd admin tenant list''', left(p_id, 64));
    END IF;
    IF v_status = 'deleted' THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'tenant %L is already deleted; nothing was changed. To bring it back run ''crosscodexd admin tenant restore''', left(p_id, 64));
    END IF;
    UPDATE public.tenants SET status = 'deleted', deleted_at = now(), deleted_by = p_actor WHERE tenant_id = p_id;
    PERFORM public.record_identity_event('delete', 'tenant', p_id, p_id, p_id, p_actor, p_reason, p_request, p_attestation);
END
$$;

-- restore_tenant brings a deleted, unhidden tenant back as suspended, so an
-- operator re-activates it deliberately with set_tenant_status.
CREATE FUNCTION public.restore_tenant(p_id text, p_actor text, p_reason text, p_request bytea, p_attestation bytea)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_status text;
    v_hidden_at timestamptz;
BEGIN
    SELECT status, hidden_at INTO v_status, v_hidden_at FROM public.tenants WHERE tenant_id = p_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = format(
            'tenant %L does not exist; nothing was changed. Check the ID with ''crosscodexd admin tenant list''', left(p_id, 64));
    END IF;
    IF v_status <> 'deleted' THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'tenant %L is not deleted (status %L); nothing was changed', left(p_id, 64), v_status);
    END IF;
    IF v_hidden_at IS NOT NULL THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'tenant %L is hidden; nothing was changed. Run ''crosscodexd admin tenant unhide'' first', left(p_id, 64));
    END IF;
    UPDATE public.tenants SET status = 'suspended', deleted_at = NULL, deleted_by = NULL WHERE tenant_id = p_id;
    PERFORM public.record_identity_event('restore', 'tenant', p_id, p_id, p_id, p_actor, p_reason, p_request, p_attestation);
END
$$;

-- hide_tenant shows a deleted tenant only by its pseudonym and returns it.
-- The pseudonym is 64 bits of a hash of a random UUID (sha256, not md5, so
-- it works under FIPS), is kept on unhide, and is reused on the next hide.
CREATE FUNCTION public.hide_tenant(p_id text, p_actor text, p_reason text, p_request bytea, p_attestation bytea)
RETURNS text
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_status text;
    v_hidden_at timestamptz;
    v_pseudonym text;
BEGIN
    SELECT status, hidden_at INTO v_status, v_hidden_at FROM public.tenants WHERE tenant_id = p_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = format(
            'tenant %L does not exist; nothing was changed. Check the ID with ''crosscodexd admin tenant list''', left(p_id, 64));
    END IF;
    IF v_status <> 'deleted' THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'only a deleted tenant can be hidden; tenant %L is %L. Delete it first with ''crosscodexd admin tenant delete''',
            left(p_id, 64), v_status);
    END IF;
    IF v_hidden_at IS NOT NULL THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'tenant %L is already hidden; nothing was changed', left(p_id, 64));
    END IF;
    UPDATE public.tenants
    SET pseudonym = COALESCE(pseudonym, 'hidden-tenant-' || left(encode(sha256(uuid_send(gen_random_uuid())), 'hex'), 16)),
        hidden_at = now()
    WHERE tenant_id = p_id
    RETURNING pseudonym INTO v_pseudonym;
    PERFORM public.record_identity_event('hide', 'tenant', p_id, p_id, p_id, p_actor, p_reason, p_request, p_attestation);
    RETURN v_pseudonym;
END
$$;

-- unhide_tenant shows a hidden tenant by its ID again; it stays deleted.
CREATE FUNCTION public.unhide_tenant(p_id text, p_actor text, p_reason text, p_request bytea, p_attestation bytea)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_hidden_at timestamptz;
BEGIN
    SELECT hidden_at INTO v_hidden_at FROM public.tenants WHERE tenant_id = p_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = format(
            'tenant %L does not exist; nothing was changed. Check the ID with ''crosscodexd admin tenant list''', left(p_id, 64));
    END IF;
    IF v_hidden_at IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'tenant %L is not hidden; nothing was changed', left(p_id, 64));
    END IF;
    UPDATE public.tenants SET hidden_at = NULL WHERE tenant_id = p_id;
    PERFORM public.record_identity_event('unhide', 'tenant', p_id, p_id, p_id, p_actor, p_reason, p_request, p_attestation);
END
$$;

REVOKE ALL ON FUNCTION public.delete_tenant(text, text, text, bytea, bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.restore_tenant(text, text, text, bytea, bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.hide_tenant(text, text, text, bytea, bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.unhide_tenant(text, text, text, bytea, bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.delete_tenant(text, text, text, bytea, bytea) TO tenant_admin;
GRANT EXECUTE ON FUNCTION public.restore_tenant(text, text, text, bytea, bytea) TO tenant_admin;
GRANT EXECUTE ON FUNCTION public.hide_tenant(text, text, text, bytea, bytea) TO tenant_admin;
GRANT EXECUTE ON FUNCTION public.unhide_tenant(text, text, text, bytea, bytea) TO tenant_admin;

-- set_tenant_status (from 007) additionally refuses a deleted tenant: only
-- restore_tenant leaves 'deleted', so the delete and restore events stay
-- paired. CREATE OR REPLACE keeps the existing grants.
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
    IF v_previous = 'deleted' THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'tenant %L is deleted; nothing was changed. Run ''crosscodexd admin tenant restore --tenant %s --by <operator> --reason <text>'' first',
            left(p_id, 64), left(p_id, 64));
    END IF;

    UPDATE public.tenants SET status = p_status WHERE tenant_id = p_id;
    RETURN v_previous;
END
$$;

-- provision_tenant (from 007) additionally refuses the reserved 'hidden-'
-- ID prefix and renaming a deleted tenant. CREATE OR REPLACE keeps the existing grants.
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
    IF p_id LIKE 'hidden-%' THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = format(
            'tenant ID %L is invalid: tenant IDs starting with ''hidden-'' are reserved for pseudonyms of hidden tenants; choose another ID',
            left(p_id, 64));
    END IF;
    IF EXISTS (SELECT 1 FROM public.tenants WHERE tenant_id = p_id AND status = 'deleted') THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'tenant %L is deleted; nothing was changed. Restore it with ''crosscodexd admin tenant restore'' before renaming it', left(p_id, 64));
    END IF;

    INSERT INTO public.tenants (tenant_id, display_name) VALUES (p_id, p_name)
        ON CONFLICT (tenant_id) DO NOTHING;
    IF FOUND THEN
        RETURN true;
    END IF;

    -- The status filter closes the race with a concurrent delete_tenant
    -- between the check above and this UPDATE; tenants are never removed,
    -- so NOT FOUND here means exactly that.
    UPDATE public.tenants SET display_name = p_name WHERE tenant_id = p_id AND status <> 'deleted';
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'tenant %L was deleted while it was being renamed; nothing was changed. Restore it with ''crosscodexd admin tenant restore'' before renaming it', left(p_id, 64));
    END IF;
    RETURN false;
END
$$;

-- list_tenants gains deleted_at and hidden. Its return type changes, so it
-- is dropped and recreated with its grant.
DROP FUNCTION public.list_tenants();
CREATE FUNCTION public.list_tenants()
RETURNS TABLE (tenant_id text, display_name text, status text, created_at timestamptz,
               deleted_at timestamptz, hidden boolean)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
    -- A hidden tenant is shown only by its pseudonym; 'crosscodexd admin
    -- identity trace' maps it back.
    SELECT CASE WHEN t.hidden_at IS NULL THEN t.tenant_id ELSE t.pseudonym END,
           CASE WHEN t.hidden_at IS NULL THEN t.display_name ELSE '' END,
           t.status, t.created_at, t.deleted_at, t.hidden_at IS NOT NULL
    FROM public.tenants t
    ORDER BY 1
$$;
REVOKE ALL ON FUNCTION public.list_tenants() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.list_tenants() TO tenant_admin;

-- enter_tenant scopes the transaction to p_id for RLS, refusing a deleted
-- tenant (fail closed). It replaces set_config('app.current_tenant', ...) on
-- every app_user and graph_user path; retention keeps set_config because job
-- data of a deleted tenant still follows retention. An unknown tenant is
-- entered as before: RLS then matches zero rows. set_config is transaction-
-- local and is not reset on return, because only search_path is a SET clause.
--
-- The check takes no lock, like suspension: a transaction that entered
-- before delete_tenant committed may finish; every later one is refused.
CREATE FUNCTION public.enter_tenant(p_id text)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM public.tenants WHERE tenant_id = p_id AND status = 'deleted') THEN
        RAISE EXCEPTION USING ERRCODE = '42501', MESSAGE = format(
            'tenant %L is deleted; its data is kept but cannot be used. To use it again an operator runs ''crosscodexd admin tenant restore --tenant %s --by <operator> --reason <text>''',
            left(p_id, 64), left(p_id, 64));
    END IF;
    PERFORM set_config('app.current_tenant', p_id, true);
END
$$;
REVOKE ALL ON FUNCTION public.enter_tenant(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.enter_tenant(text) TO app_user, graph_user;

-- reviewers registers, per tenant, the people who record human verdicts:
-- determined_by on a human verdict holds reviewer_id. Rows are soft-deleted
-- and hidden like tenants. app_user reads it to show a hidden reviewer's
-- pseudonym; every change goes through the tenant_admin functions below.
-- reviewers_id_format mirrors artifacts.ValidateReviewer (maxReviewerBytes
-- = 256); Go stays the boundary check that also refuses format characters.
CREATE TABLE public.reviewers (
    tenant_id   text NOT NULL REFERENCES public.tenants (tenant_id) ON DELETE RESTRICT,
    reviewer_id text NOT NULL,
    status      text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'deleted')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    created_by  text NOT NULL,
    deleted_at  timestamptz,
    deleted_by  text,
    pseudonym   text UNIQUE,
    hidden_at   timestamptz,
    PRIMARY KEY (tenant_id, reviewer_id),
    CONSTRAINT reviewers_id_format
        CHECK (reviewer_id !~ '^\s*$' AND octet_length(reviewer_id) <= 256
               AND reviewer_id !~ '[\u0001-\u001F\u007F-\u009F]' AND reviewer_id = btrim(reviewer_id)),
    -- IDs starting with 'hidden-' are reserved, so output can never show a
    -- real reviewer ID that looks like a pseudonym.
    CONSTRAINT reviewers_id_not_reserved CHECK (reviewer_id NOT LIKE 'hidden-%'),
    CONSTRAINT reviewers_pseudonym_format
        CHECK (pseudonym IS NULL OR pseudonym ~ '^hidden-reviewer-[0-9a-f]{16}$'),
    CONSTRAINT reviewers_deleted_consistent
        CHECK ((status = 'deleted') = (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)),
    CONSTRAINT reviewers_hidden_consistent
        CHECK (hidden_at IS NULL OR (status = 'deleted' AND pseudonym IS NOT NULL))
);
ALTER TABLE public.reviewers ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON public.reviewers
    USING (tenant_id = current_setting('app.current_tenant', true));
GRANT SELECT ON public.reviewers TO app_user;
CREATE TRIGGER reviewers_refuse_delete
    BEFORE DELETE ON public.reviewers
    FOR EACH ROW EXECUTE FUNCTION public.refuse_change(
        'reviewers are never deleted because their judgements are permanent. Run ''crosscodexd admin reviewer delete'' to soft-delete instead.');
CREATE TRIGGER reviewers_refuse_truncate
    BEFORE TRUNCATE ON public.reviewers
    FOR EACH STATEMENT EXECUTE FUNCTION public.refuse_change(
        'reviewers are never deleted because their judgements are permanent, so TRUNCATE is refused. Run ''crosscodexd admin reviewer delete'' to soft-delete each reviewer instead.');

-- Every reviewer who already recorded a human verdict is registered, so
-- existing reviewers can keep deciding after this migration. The check at
-- the top of this file guarantees every ID satisfies the constraints.
INSERT INTO public.reviewers (tenant_id, reviewer_id, created_by)
SELECT DISTINCT tenant_id, determined_by, 'migration-009'
FROM public.artifact_pair_verdicts
WHERE determination_type = 'human'
ON CONFLICT DO NOTHING;

-- add_reviewer registers a reviewer and returns true, or returns false when
-- the reviewer is already active (upsert over reject); both are recorded.
-- A deleted reviewer is refused: restore_reviewer brings it back.
--
-- On a deleted tenant, add_reviewer and restore_reviewer are refused, since
-- they would make a reviewer usable again; delete, hide and unhide stay
-- allowed so a deleted tenant's reviewers can still be retired and concealed.
CREATE FUNCTION public.add_reviewer(p_tenant text, p_reviewer text, p_actor text, p_reason text, p_request bytea, p_attestation bytea)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_tenant_status text;
    v_status text;
    v_inserted boolean;
BEGIN
    -- FOR SHARE keeps delete_tenant from running until this commits.
    SELECT status INTO v_tenant_status FROM public.tenants WHERE tenant_id = p_tenant FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = format(
            'tenant %L does not exist; nothing was changed. Check the ID with ''crosscodexd admin tenant list''', left(p_tenant, 64));
    END IF;
    IF v_tenant_status = 'deleted' THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'tenant %L is deleted; nothing was changed. To add reviewers, restore the tenant first with ''crosscodexd admin tenant restore''',
            left(p_tenant, 64));
    END IF;
    IF p_reviewer LIKE 'hidden-%' THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE = format(
            'reviewer ID %L is invalid: reviewer IDs starting with ''hidden-'' are reserved for pseudonyms of hidden reviewers; nothing was changed. Choose another ID',
            left(p_reviewer, 64));
    END IF;
    SELECT status INTO v_status FROM public.reviewers
    WHERE tenant_id = p_tenant AND reviewer_id = p_reviewer FOR UPDATE;
    IF v_status = 'deleted' THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'reviewer %L of tenant %L is deleted; nothing was changed. Run ''crosscodexd admin reviewer restore'' instead',
            left(p_reviewer, 64), left(p_tenant, 64));
    END IF;
    BEGIN
        INSERT INTO public.reviewers (tenant_id, reviewer_id, created_by)
        VALUES (p_tenant, p_reviewer, p_actor)
        ON CONFLICT (tenant_id, reviewer_id) DO NOTHING;
        v_inserted := FOUND;
    EXCEPTION WHEN check_violation OR not_null_violation THEN
        RAISE EXCEPTION USING ERRCODE = '22023', MESSAGE =
            'reviewer ID must be 1-256 bytes of printable text without leading or trailing spaces; nothing was changed';
    END;
    PERFORM public.record_identity_event('add', 'reviewer', p_tenant, p_reviewer, p_reviewer, p_actor, p_reason, p_request, p_attestation);
    RETURN v_inserted;
END
$$;

-- delete_reviewer soft-deletes a reviewer: their verdicts stay, and
-- require_active_reviewer refuses them new ones.
CREATE FUNCTION public.delete_reviewer(p_tenant text, p_reviewer text, p_actor text, p_reason text, p_request bytea, p_attestation bytea)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_status text;
BEGIN
    SELECT status INTO v_status FROM public.reviewers
    WHERE tenant_id = p_tenant AND reviewer_id = p_reviewer FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = format(
            'reviewer %L is not registered for tenant %L; nothing was changed. Register them with ''crosscodexd admin reviewer add''',
            left(p_reviewer, 64), left(p_tenant, 64));
    END IF;
    IF v_status = 'deleted' THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'reviewer %L of tenant %L is already deleted; nothing was changed. To bring them back run ''crosscodexd admin reviewer restore''',
            left(p_reviewer, 64), left(p_tenant, 64));
    END IF;
    UPDATE public.reviewers SET status = 'deleted', deleted_at = now(), deleted_by = p_actor
    WHERE tenant_id = p_tenant AND reviewer_id = p_reviewer;
    PERFORM public.record_identity_event('delete', 'reviewer', p_tenant, p_reviewer, p_reviewer, p_actor, p_reason, p_request, p_attestation);
END
$$;

-- restore_reviewer returns a deleted, unhidden reviewer to active (a
-- reviewer has no suspended state).
CREATE FUNCTION public.restore_reviewer(p_tenant text, p_reviewer text, p_actor text, p_reason text, p_request bytea, p_attestation bytea)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_status text;
    v_hidden_at timestamptz;
    v_tenant_status text;
BEGIN
    SELECT status, hidden_at INTO v_status, v_hidden_at FROM public.reviewers
    WHERE tenant_id = p_tenant AND reviewer_id = p_reviewer FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = format(
            'reviewer %L is not registered for tenant %L; nothing was changed. Register them with ''crosscodexd admin reviewer add''',
            left(p_reviewer, 64), left(p_tenant, 64));
    END IF;
    -- FOR SHARE keeps delete_tenant from running until this commits.
    SELECT status INTO v_tenant_status FROM public.tenants WHERE tenant_id = p_tenant FOR SHARE;
    IF v_tenant_status = 'deleted' THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'tenant %L is deleted; nothing was changed. To restore its reviewers, restore the tenant first with ''crosscodexd admin tenant restore''',
            left(p_tenant, 64));
    END IF;
    IF v_status <> 'deleted' THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'reviewer %L of tenant %L is not deleted (status %L); nothing was changed',
            left(p_reviewer, 64), left(p_tenant, 64), v_status);
    END IF;
    IF v_hidden_at IS NOT NULL THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'reviewer %L of tenant %L is hidden; nothing was changed. Run ''crosscodexd admin reviewer unhide'' first',
            left(p_reviewer, 64), left(p_tenant, 64));
    END IF;
    UPDATE public.reviewers SET status = 'active', deleted_at = NULL, deleted_by = NULL
    WHERE tenant_id = p_tenant AND reviewer_id = p_reviewer;
    PERFORM public.record_identity_event('restore', 'reviewer', p_tenant, p_reviewer, p_reviewer, p_actor, p_reason, p_request, p_attestation);
END
$$;

-- hide_reviewer shows a deleted reviewer only by its pseudonym and returns
-- it. The pseudonym is generated as for tenants, kept on unhide, and reused
-- on the next hide.
CREATE FUNCTION public.hide_reviewer(p_tenant text, p_reviewer text, p_actor text, p_reason text, p_request bytea, p_attestation bytea)
RETURNS text
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_status text;
    v_hidden_at timestamptz;
    v_pseudonym text;
BEGIN
    SELECT status, hidden_at INTO v_status, v_hidden_at FROM public.reviewers
    WHERE tenant_id = p_tenant AND reviewer_id = p_reviewer FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = format(
            'reviewer %L is not registered for tenant %L; nothing was changed. Register them with ''crosscodexd admin reviewer add''',
            left(p_reviewer, 64), left(p_tenant, 64));
    END IF;
    IF v_status <> 'deleted' THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'only a deleted reviewer can be hidden; reviewer %L of tenant %L is %L. Delete them first with ''crosscodexd admin reviewer delete''',
            left(p_reviewer, 64), left(p_tenant, 64), v_status);
    END IF;
    IF v_hidden_at IS NOT NULL THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'reviewer %L of tenant %L is already hidden; nothing was changed', left(p_reviewer, 64), left(p_tenant, 64));
    END IF;
    UPDATE public.reviewers
    SET pseudonym = COALESCE(pseudonym, 'hidden-reviewer-' || left(encode(sha256(uuid_send(gen_random_uuid())), 'hex'), 16)),
        hidden_at = now()
    WHERE tenant_id = p_tenant AND reviewer_id = p_reviewer
    RETURNING pseudonym INTO v_pseudonym;
    PERFORM public.record_identity_event('hide', 'reviewer', p_tenant, p_reviewer, p_reviewer, p_actor, p_reason, p_request, p_attestation);
    RETURN v_pseudonym;
END
$$;

-- unhide_reviewer shows a hidden reviewer by their ID again; they stay deleted.
CREATE FUNCTION public.unhide_reviewer(p_tenant text, p_reviewer text, p_actor text, p_reason text, p_request bytea, p_attestation bytea)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_hidden_at timestamptz;
BEGIN
    SELECT hidden_at INTO v_hidden_at FROM public.reviewers
    WHERE tenant_id = p_tenant AND reviewer_id = p_reviewer FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = format(
            'reviewer %L is not registered for tenant %L; nothing was changed. Register them with ''crosscodexd admin reviewer add''',
            left(p_reviewer, 64), left(p_tenant, 64));
    END IF;
    IF v_hidden_at IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = format(
            'reviewer %L of tenant %L is not hidden; nothing was changed', left(p_reviewer, 64), left(p_tenant, 64));
    END IF;
    UPDATE public.reviewers SET hidden_at = NULL WHERE tenant_id = p_tenant AND reviewer_id = p_reviewer;
    PERFORM public.record_identity_event('unhide', 'reviewer', p_tenant, p_reviewer, p_reviewer, p_actor, p_reason, p_request, p_attestation);
END
$$;

-- list_reviewers lists a tenant's reviewers; a hidden reviewer is shown
-- only by its pseudonym.
CREATE FUNCTION public.list_reviewers(p_tenant text)
RETURNS TABLE (reviewer_id text, status text, created_at timestamptz, created_by text,
               deleted_at timestamptz, hidden boolean)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT CASE WHEN r.hidden_at IS NULL THEN r.reviewer_id ELSE r.pseudonym END,
           r.status, r.created_at, r.created_by, r.deleted_at, r.hidden_at IS NOT NULL
    FROM public.reviewers r
    WHERE r.tenant_id = p_tenant
    ORDER BY 1
$$;

-- trace_pseudonym maps a tenant or reviewer pseudonym back to the identity
-- and records the lookup, because revealing a hidden identity is itself an
-- audited action. An unknown pseudonym records nothing.
CREATE FUNCTION public.trace_pseudonym(p_pseudonym text, p_actor text, p_reason text, p_request bytea, p_attestation bytea)
RETURNS TABLE (kind text, tenant_id text, identity_id text, status text, hidden boolean)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_kind text;
    v_tenant text;
    v_identity text;
    v_status text;
    v_hidden boolean;
BEGIN
    -- Pseudonyms are unique per table and the prefix names the table, so at
    -- most one row matches.
    SELECT m.kind, m.tenant_id, m.identity_id, m.status, m.hidden
    INTO v_kind, v_tenant, v_identity, v_status, v_hidden
    FROM (SELECT 'tenant'::text AS kind, t.tenant_id, t.tenant_id AS identity_id, t.status,
                 t.hidden_at IS NOT NULL AS hidden
          FROM public.tenants t WHERE t.pseudonym = p_pseudonym
          UNION ALL
          SELECT 'reviewer'::text, r.tenant_id, r.reviewer_id, r.status, r.hidden_at IS NOT NULL
          FROM public.reviewers r WHERE r.pseudonym = p_pseudonym) m;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = format(
            'no tenant or reviewer has pseudonym %L; nothing was recorded. Check the pseudonym in the output that showed it',
            left(p_pseudonym, 64));
    END IF;
    -- p_subject is the pseudonym: a trace request names it, not the identity.
    PERFORM public.record_identity_event('trace', v_kind, v_tenant, v_identity, p_pseudonym, p_actor, p_reason, p_request, p_attestation);
    RETURN QUERY SELECT v_kind, v_tenant, v_identity, v_status, v_hidden;
END
$$;

-- require_active_reviewer locks the current tenant's reviewer row FOR SHARE
-- until the caller's transaction ends, so delete_reviewer cannot run while a
-- verdict by that reviewer is being recorded. It is a definer function
-- because FOR SHARE needs UPDATE privilege, which app_user must not have.
--
-- The tenant comes from app.current_tenant, which app_user can set directly
-- with set_config, so this function trusts it exactly as RLS does: it
-- reveals no more than app_user can already SELECT from reviewers. Residual
-- risk: app_user can take FOR SHARE on another tenant's reviewer row and
-- delay that tenant's delete_reviewer until its own transaction ends. It
-- re-checks that the tenant is not deleted, because a direct set_config
-- skips enter_tenant's check. The reviewer messages stay terse: pkg callers
-- map the SQLSTATE (P0002 unregistered, 55000 deleted) to actionable errors.
CREATE FUNCTION public.require_active_reviewer(p_reviewer text)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_tenant text := current_setting('app.current_tenant', true);
    v_status text;
BEGIN
    IF v_tenant IS NULL OR v_tenant = '' THEN
        RAISE EXCEPTION USING ERRCODE = '42501', MESSAGE =
            'require_active_reviewer needs a tenant transaction: call enter_tenant first';
    END IF;
    IF EXISTS (SELECT 1 FROM public.tenants t WHERE t.tenant_id = v_tenant AND t.status = 'deleted') THEN
        RAISE EXCEPTION USING ERRCODE = '42501', MESSAGE = format(
            'tenant %L is deleted; its data is kept but cannot be used. To use it again an operator runs ''crosscodexd admin tenant restore --tenant %s --by <operator> --reason <text>''',
            left(v_tenant, 64), left(v_tenant, 64));
    END IF;
    SELECT r.status INTO v_status FROM public.reviewers r
    WHERE r.tenant_id = v_tenant AND r.reviewer_id = p_reviewer FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION USING ERRCODE = 'P0002', MESSAGE = 'reviewer not registered';
    END IF;
    IF v_status = 'deleted' THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = 'reviewer deleted';
    END IF;
END
$$;

REVOKE ALL ON FUNCTION public.add_reviewer(text, text, text, text, bytea, bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.delete_reviewer(text, text, text, text, bytea, bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.restore_reviewer(text, text, text, text, bytea, bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.hide_reviewer(text, text, text, text, bytea, bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.unhide_reviewer(text, text, text, text, bytea, bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.list_reviewers(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.trace_pseudonym(text, text, text, bytea, bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.require_active_reviewer(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.add_reviewer(text, text, text, text, bytea, bytea) TO tenant_admin;
GRANT EXECUTE ON FUNCTION public.delete_reviewer(text, text, text, text, bytea, bytea) TO tenant_admin;
GRANT EXECUTE ON FUNCTION public.restore_reviewer(text, text, text, text, bytea, bytea) TO tenant_admin;
GRANT EXECUTE ON FUNCTION public.hide_reviewer(text, text, text, text, bytea, bytea) TO tenant_admin;
GRANT EXECUTE ON FUNCTION public.unhide_reviewer(text, text, text, text, bytea, bytea) TO tenant_admin;
GRANT EXECUTE ON FUNCTION public.list_reviewers(text) TO tenant_admin;
GRANT EXECUTE ON FUNCTION public.trace_pseudonym(text, text, text, bytea, bytea) TO tenant_admin;
GRANT EXECUTE ON FUNCTION public.require_active_reviewer(text) TO app_user;

-- list_retention_tenants lists every tenant for 'crosscodexd admin retention
-- scan --all-tenants': a deleted tenant's job data still follows retention.
-- tenant_id is real, to scope the scan; display_id is the pseudonym of a
-- hidden tenant, else the tenant_id, and is the only column to print. The
-- caller already holds tenant_admin, which can run delete_tenant and
-- trace_pseudonym, so exposing real IDs here widens nothing.
CREATE FUNCTION public.list_retention_tenants()
RETURNS TABLE (tenant_id text, display_id text, status text)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT t.tenant_id,
           CASE WHEN t.hidden_at IS NULL THEN t.tenant_id ELSE t.pseudonym END,
           t.status
    FROM public.tenants t
    ORDER BY 1
$$;
REVOKE ALL ON FUNCTION public.list_retention_tenants() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.list_retention_tenants() TO tenant_admin;
