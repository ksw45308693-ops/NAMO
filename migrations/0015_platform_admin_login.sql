UPDATE public.users
SET email = 'admin'
WHERE email = 'admin@namo.invalid'
  AND role = 'platform_admin'
  AND tenant_id IS NULL;
