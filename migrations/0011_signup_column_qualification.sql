-- The signup functions declare RETURNS TABLE output names that also exist as
-- users columns (user_id, tenant_id, email, role). plpgsql treats an
-- unqualified reference to such a name as ambiguous at run time, so every
-- column reference below is qualified with its table alias.

CREATE OR REPLACE FUNCTION public.signup_create_account(p_email text, p_password_hash text)
RETURNS TABLE (user_id uuid, tenant_id uuid, email text, role text)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE
    v_email text := lower(btrim(p_email));
    v_display_name text;
    v_user_id uuid;
BEGIN
    IF v_email !~ '^[^[:space:]@]+@[^[:space:]@]+$' OR octet_length(v_email) > 320 THEN
        RAISE EXCEPTION 'valid email address is required' USING ERRCODE = '22023';
    END IF;
    IF coalesce(length(p_password_hash), 0) < 59 OR left(p_password_hash, 2) <> '$2' THEN
        RAISE EXCEPTION 'valid password hash is required' USING ERRCODE = '22023';
    END IF;
    v_display_name := coalesce(nullif(btrim(split_part(v_email, '@', 1)), ''), '신규 사용자');

    -- The invitation flow reserves an email with the same key, so signup and
    -- invitation acceptance can never create two accounts for one address.
    PERFORM pg_catalog.pg_advisory_xact_lock(
        pg_catalog.hashtextextended('namo-invitation:' || v_email, 0)
    );
    IF EXISTS (SELECT 1 FROM public.users existing WHERE lower(existing.email) = v_email) THEN
        RAISE EXCEPTION 'email already belongs to an account' USING ERRCODE = '23505';
    END IF;
    IF EXISTS (
        SELECT 1 FROM public.invitations pending
        WHERE lower(pending.email) = v_email
          AND pending.accepted_at IS NULL
          AND pending.expires_at > clock_timestamp()
    ) THEN
        RAISE EXCEPTION 'invitation already pending' USING ERRCODE = 'P0001';
    END IF;

    INSERT INTO public.users (tenant_id, email, display_name, password_hash, role)
    VALUES (NULL, v_email, v_display_name, p_password_hash, 'member')
    RETURNING users.id INTO v_user_id;

    RETURN QUERY SELECT v_user_id, NULL::uuid, v_email, 'member'::text;
END;
$$;

CREATE OR REPLACE FUNCTION public.signup_member_accounts(p_actor_user_id uuid)
RETURNS TABLE (user_id uuid, email text, display_name text, tenant_id uuid, tenant_name text, created_at timestamptz)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM public.users actor
        WHERE actor.id = p_actor_user_id AND actor.tenant_id IS NULL AND actor.role = 'platform_admin'
    ) THEN
        RAISE EXCEPTION 'platform administrator role is required' USING ERRCODE = '42501';
    END IF;
    RETURN QUERY
    SELECT member.id, member.email, member.display_name, member.tenant_id,
           coalesce(owner.name, ''), member.created_at
    FROM public.users member
    LEFT JOIN public.tenants owner ON owner.id = member.tenant_id
    WHERE member.role = 'member'
    ORDER BY (member.tenant_id IS NOT NULL), member.created_at DESC, member.id;
END;
$$;

CREATE OR REPLACE FUNCTION public.signup_set_account_tenant(p_actor_user_id uuid, p_user_id uuid, p_tenant_id uuid)
RETURNS TABLE (user_id uuid, tenant_id uuid, email text, role text)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE v_email text;
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
    IF p_tenant_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM public.tenants target WHERE target.id = p_tenant_id
    ) THEN
        RAISE EXCEPTION 'tenant is unavailable' USING ERRCODE = 'P0002';
    END IF;

    -- Only member accounts move between tenants. Platform and tenant
    -- administrators keep the scope their onboarding established.
    UPDATE public.users member SET tenant_id = p_tenant_id
    WHERE member.id = p_user_id AND member.role = 'member'
    RETURNING member.email INTO v_email;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'member account is unavailable' USING ERRCODE = 'P0002';
    END IF;

    RETURN QUERY SELECT p_user_id, p_tenant_id, v_email, 'member'::text;
END;
$$;
