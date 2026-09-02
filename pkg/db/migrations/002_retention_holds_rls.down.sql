DROP POLICY IF EXISTS tenant_isolation ON public.retention_holds;
ALTER TABLE public.retention_holds DISABLE ROW LEVEL SECURITY;
