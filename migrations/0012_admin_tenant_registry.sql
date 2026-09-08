-- Platform administrators register a company directly: name, contact email,
-- and the administrator contact. No invitation and no mail delivery are
-- involved, so accounts reach a company only through tenant assignment.

ALTER TABLE public.tenants ADD COLUMN admin_name text NOT NULL DEFAULT '';
ALTER TABLE public.tenants ADD COLUMN admin_email text NOT NULL DEFAULT '';
ALTER TABLE public.tenants ADD CONSTRAINT tenants_admin_contact_length
    CHECK (octet_length(admin_name) <= 512 AND octet_length(admin_email) <= 320);

GRANT SELECT, INSERT ON TABLE public.tenants, public.schedules TO namo_signup_definer;

CREATE FUNCTION public.admin_register_tenant(
    p_actor_user_id uuid,
    p_name text,
    p_contact_email text,
    p_admin_name text,
    p_admin_email text
)
RETURNS uuid
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
    v_name text := btrim(p_name);
    v_contact_email text := lower(btrim(p_contact_email));
    v_admin_name text := btrim(p_admin_name);
    v_admin_email text := lower(btrim(p_admin_email));
    v_tenant_id uuid;
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM public.users actor
        WHERE actor.id = p_actor_user_id AND actor.tenant_id IS NULL AND actor.role = 'platform_admin'
    ) THEN
        RAISE EXCEPTION 'platform administrator role is required' USING ERRCODE = '42501';
    END IF;
    IF coalesce(octet_length(v_name), 0) = 0 OR octet_length(v_name) > 512
       OR coalesce(octet_length(v_admin_name), 0) = 0 OR octet_length(v_admin_name) > 512 THEN
        RAISE EXCEPTION 'company and administrator names are required' USING ERRCODE = '22023';
    END IF;
    IF v_contact_email !~ '^[^[:space:]@]+@[^[:space:]@]+$' OR octet_length(v_contact_email) > 320
       OR v_admin_email !~ '^[^[:space:]@]+@[^[:space:]@]+$' OR octet_length(v_admin_email) > 320 THEN
        RAISE EXCEPTION 'valid email addresses are required' USING ERRCODE = '22023';
    END IF;

    PERFORM pg_catalog.pg_advisory_xact_lock(
        pg_catalog.hashtextextended('namo-tenant-registry:' || lower(v_name) || '|' || v_contact_email, 0)
    );
    IF EXISTS (
        SELECT 1 FROM public.tenants existing
        WHERE lower(btrim(existing.name)) = lower(v_name)
          AND lower(btrim(existing.contact_email)) = v_contact_email
    ) THEN
        RAISE EXCEPTION 'tenant is already registered' USING ERRCODE = '23505';
    END IF;

    INSERT INTO public.tenants (name, contact_email, admin_name, admin_email)
    VALUES (v_name, v_contact_email, v_admin_name, v_admin_email)
    RETURNING tenants.id INTO v_tenant_id;

    INSERT INTO public.schedules (tenant_id, name, hour, minute, timezone, weekdays)
    VALUES (v_tenant_id, '기본 알림', 7, 0, 'Asia/Seoul', ARRAY[0,1,2,3,4,5,6]::smallint[])
    ON CONFLICT (tenant_id, name) DO NOTHING;

    RETURN v_tenant_id;
END;
$$;

CREATE FUNCTION public.admin_tenant_registry(p_actor_user_id uuid)
RETURNS TABLE (tenant_id uuid, name text, contact_email text, admin_name text, admin_email text, member_count bigint, created_at timestamptz)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
#variable_conflict use_column
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM public.users actor
        WHERE actor.id = p_actor_user_id AND actor.tenant_id IS NULL AND actor.role = 'platform_admin'
    ) THEN
        RAISE EXCEPTION 'platform administrator role is required' USING ERRCODE = '42501';
    END IF;
    RETURN QUERY
    SELECT company.id, company.name, company.contact_email, company.admin_name, company.admin_email,
           (SELECT count(*) FROM public.users seat WHERE seat.tenant_id = company.id),
           company.created_at
    FROM public.tenants company
    ORDER BY company.created_at, company.id;
END;
$$;

ALTER FUNCTION public.admin_register_tenant(uuid, text, text, text, text) OWNER TO namo_signup_definer;
ALTER FUNCTION public.admin_tenant_registry(uuid) OWNER TO namo_signup_definer;

REVOKE ALL ON FUNCTION public.admin_register_tenant(uuid, text, text, text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.admin_tenant_registry(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.admin_register_tenant(uuid, text, text, text, text) TO namo_runtime;
GRANT EXECUTE ON FUNCTION public.admin_tenant_registry(uuid) TO namo_runtime;
