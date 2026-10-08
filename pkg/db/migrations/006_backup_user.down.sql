DROP POLICY IF EXISTS backup_list_tenants ON public.tenants;
REVOKE SELECT (tenant_id) ON public.tenants FROM backup_user;
REVOKE pg_read_all_settings FROM backup_user;
DROP ROLE IF EXISTS backup_user;
