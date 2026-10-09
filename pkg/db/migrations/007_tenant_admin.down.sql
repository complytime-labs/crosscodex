DROP FUNCTION IF EXISTS public.tenant_is_active(text);
DROP FUNCTION IF EXISTS public.list_tenants();
DROP FUNCTION IF EXISTS public.set_tenant_status(text, text);
DROP FUNCTION IF EXISTS public.provision_tenant(text, text);
DROP ROLE IF EXISTS tenant_admin;
ALTER TABLE public.tenants DROP CONSTRAINT IF EXISTS tenants_status_check;
