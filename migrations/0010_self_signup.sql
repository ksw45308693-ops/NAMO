-- Self-service signup. A new account starts as a member without a tenant and
-- stays read-only until a platform administrator assigns one.

DO $$
DECLARE constraint_name text;
BEGIN
    FOR constraint_name IN
        SELECT c.conname
        FROM pg_catalog.pg_constraint c
        WHERE c.conrelid = 'public.users'::regclass
          AND c.contype = 'c'
          AND pg_catalog.pg_get_constraintdef(c.oid) LIKE '%platform_admin%tenant_id IS NULL%'
    LOOP
        EXECUTE pg_catalog.format('ALTER TABLE public.users DROP CONSTRAINT %I', constraint_name);
    END LOOP;
END $$;

ALTER TABLE public.users ADD CONSTRAINT users_role_tenant_scope CHECK (
    (role = 'platform_admin' AND tenant_id IS NULL)
    OR (role = 'tenant_admin' AND tenant_id IS NOT NULL)
    OR (role = 'member')
);

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = 'namo_signup_definer') THEN
        CREATE ROLE namo_signup_definer NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION BYPASSRLS NOINHERIT;
    END IF;
END $$;
ALTER ROLE namo_signup_definer NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION BYPASSRLS NOINHERIT;

DO $$
DECLARE parent_role record;
BEGIN
    FOR parent_role IN
        SELECT parent.rolname
        FROM pg_catalog.pg_auth_members membership
        JOIN pg_catalog.pg_roles parent ON parent.oid = membership.roleid
        JOIN pg_catalog.pg_roles member_role ON member_role.oid = membership.member
        WHERE member_role.rolname = 'namo_signup_definer'
    LOOP
        EXECUTE pg_catalog.format('REVOKE %I FROM namo_signup_definer', parent_role.rolname);
    END LOOP;
END $$;

REVOKE ALL ON ALL TABLES IN SCHEMA public FROM namo_signup_definer;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM namo_signup_definer;
GRANT USAGE ON SCHEMA public TO namo_signup_definer;
GRANT SELECT, INSERT, UPDATE ON TABLE public.users TO namo_signup_definer;
GRANT SELECT ON TABLE public.tenants TO namo_signup_definer;
GRANT SELECT ON TABLE public.invitations TO namo_signup_definer;

CREATE FUNCTION public.signup_create_account(p_email text, p_password_hash text)
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
    IF EXISTS (SELECT 1 FROM public.users WHERE lower(email) = v_email) THEN
        RAISE EXCEPTION 'email already belongs to an account' USING ERRCODE = '23505';
    END IF;
    IF EXISTS (
        SELECT 1 FROM public.invitations
        WHERE lower(email) = v_email AND accepted_at IS NULL AND expires_at > clock_timestamp()
    ) THEN
        RAISE EXCEPTION 'invitation already pending' USING ERRCODE = 'P0001';
    END IF;

    INSERT INTO public.users (tenant_id, email, display_name, password_hash, role)
    VALUES (NULL, v_email, v_display_name, p_password_hash, 'member')
    RETURNING id INTO v_user_id;

    RETURN QUERY SELECT v_user_id, NULL::uuid, v_email, 'member'::text;
END;
$$;

CREATE FUNCTION public.signup_member_accounts(p_actor_user_id uuid)
RETURNS TABLE (user_id uuid, email text, display_name text, tenant_id uuid, tenant_name text, created_at timestamptz)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM public.users
        WHERE id = p_actor_user_id AND tenant_id IS NULL AND role = 'platform_admin'
    ) THEN
        RAISE EXCEPTION 'platform administrator role is required' USING ERRCODE = '42501';
    END IF;
    RETURN QUERY
    SELECT u.id, u.email, u.display_name, u.tenant_id, coalesce(t.name, ''), u.created_at
    FROM public.users u
    LEFT JOIN public.tenants t ON t.id = u.tenant_id
    WHERE u.role = 'member'
    ORDER BY (u.tenant_id IS NOT NULL), u.created_at DESC, u.id;
END;
$$;

CREATE FUNCTION public.signup_set_account_tenant(p_actor_user_id uuid, p_user_id uuid, p_tenant_id uuid)
RETURNS TABLE (user_id uuid, tenant_id uuid, email text, role text)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE v_email text;
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM public.users
        WHERE id = p_actor_user_id AND tenant_id IS NULL AND role = 'platform_admin'
    ) THEN
        RAISE EXCEPTION 'platform administrator role is required' USING ERRCODE = '42501';
    END IF;
    IF p_user_id IS NULL THEN
        RAISE EXCEPTION 'target account is required' USING ERRCODE = '22023';
    END IF;
    IF p_tenant_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM public.tenants WHERE id = p_tenant_id) THEN
        RAISE EXCEPTION 'tenant is unavailable' USING ERRCODE = 'P0002';
    END IF;

    -- Only member accounts move between tenants. Platform and tenant
    -- administrators keep the scope their onboarding established.
    UPDATE public.users SET tenant_id = p_tenant_id
    WHERE id = p_user_id AND role = 'member'
    RETURNING email INTO v_email;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'member account is unavailable' USING ERRCODE = 'P0002';
    END IF;

    RETURN QUERY SELECT p_user_id, p_tenant_id, v_email, 'member'::text;
END;
$$;

ALTER FUNCTION public.signup_create_account(text, text) OWNER TO namo_signup_definer;
ALTER FUNCTION public.signup_member_accounts(uuid) OWNER TO namo_signup_definer;
ALTER FUNCTION public.signup_set_account_tenant(uuid, uuid, uuid) OWNER TO namo_signup_definer;

REVOKE ALL ON FUNCTION public.signup_create_account(text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.signup_member_accounts(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.signup_set_account_tenant(uuid, uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.signup_create_account(text, text) TO namo_runtime;
GRANT EXECUTE ON FUNCTION public.signup_member_accounts(uuid) TO namo_runtime;
GRANT EXECUTE ON FUNCTION public.signup_set_account_tenant(uuid, uuid, uuid) TO namo_runtime;
