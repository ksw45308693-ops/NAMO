-- Platform administrators grant company access in one step: the tenant and the
-- role inside it. A tenant_admin can change filters, settings and reports; a
-- member only reads. Platform administrators are never a target.

DROP FUNCTION public.signup_set_account_tenant(uuid, uuid, uuid);
DROP FUNCTION public.signup_member_accounts(uuid);

CREATE FUNCTION public.admin_account_registry(p_actor_user_id uuid)
RETURNS TABLE (
    user_id uuid,
    email text,
    display_name text,
    role text,
    tenant_id uuid,
    tenant_name text,
    created_at timestamptz
)
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
    SELECT seat.id, seat.email, seat.display_name, seat.role, seat.tenant_id,
           coalesce(company.name, ''), seat.created_at
    FROM public.users seat
    LEFT JOIN public.tenants company ON company.id = seat.tenant_id
    WHERE seat.role IN ('member', 'tenant_admin')
    ORDER BY (seat.tenant_id IS NOT NULL), seat.created_at DESC, seat.id;
END;
$$;

CREATE FUNCTION public.admin_set_account_access(
    p_actor_user_id uuid,
    p_user_id uuid,
    p_tenant_id uuid,
    p_role text
)
RETURNS TABLE (user_id uuid, tenant_id uuid, email text, role text)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
    v_email text;
    v_role text := btrim(coalesce(p_role, ''));
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM public.users actor
        WHERE actor.id = p_actor_user_id AND actor.tenant_id IS NULL AND actor.role = 'platform_admin'
    ) THEN
        RAISE EXCEPTION 'platform administrator role is required' USING ERRCODE = '42501';
    END IF;
    IF p_user_id IS NULL THEN
        RAISE EXCEPTION 'target account is required' USING ERRCODE = '22023';
    END IF;
    IF v_role NOT IN ('member', 'tenant_admin') THEN
        RAISE EXCEPTION 'account role must be member or tenant_admin' USING ERRCODE = '22023';
    END IF;
    -- Company access is what a role grants, so an administrator role without a
    -- company is rejected instead of silently downgraded.
    IF p_tenant_id IS NULL AND v_role <> 'member' THEN
        RAISE EXCEPTION 'a company is required for the tenant_admin role' USING ERRCODE = '22023';
    END IF;
    IF p_tenant_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM public.tenants target WHERE target.id = p_tenant_id
    ) THEN
        RAISE EXCEPTION 'tenant is unavailable' USING ERRCODE = 'P0002';
    END IF;

    UPDATE public.users seat SET tenant_id = p_tenant_id, role = v_role
    WHERE seat.id = p_user_id AND seat.role IN ('member', 'tenant_admin')
    RETURNING seat.email INTO v_email;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'member account is unavailable' USING ERRCODE = 'P0002';
    END IF;

    RETURN QUERY SELECT p_user_id, p_tenant_id, v_email, v_role;
END;
$$;

ALTER FUNCTION public.admin_account_registry(uuid) OWNER TO namo_signup_definer;
ALTER FUNCTION public.admin_set_account_access(uuid, uuid, uuid, text) OWNER TO namo_signup_definer;

REVOKE ALL ON FUNCTION public.admin_account_registry(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.admin_set_account_access(uuid, uuid, uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.admin_account_registry(uuid) TO namo_runtime;
GRANT EXECUTE ON FUNCTION public.admin_set_account_access(uuid, uuid, uuid, text) TO namo_runtime;
