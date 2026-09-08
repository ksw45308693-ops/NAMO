-- Two removal paths with separate authority. A company administrator drops a
-- member from the company and the account survives as unassigned. A platform
-- administrator deletes the account itself, which cascades its sessions.

GRANT DELETE ON TABLE public.users TO namo_signup_definer;

CREATE FUNCTION public.tenant_remove_member(
    p_actor_user_id uuid,
    p_tenant_id uuid,
    p_user_id uuid
)
RETURNS text
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE v_email text;
BEGIN
    IF p_tenant_id IS NULL OR p_user_id IS NULL THEN
        RAISE EXCEPTION 'target account is required' USING ERRCODE = '22023';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM public.users actor
        WHERE actor.id = p_actor_user_id AND actor.tenant_id = p_tenant_id AND actor.role = 'tenant_admin'
    ) THEN
        RAISE EXCEPTION 'company administrator role is required' USING ERRCODE = '42501';
    END IF;
    -- Removing yourself would leave the company without an administrator.
    IF p_actor_user_id = p_user_id THEN
        RAISE EXCEPTION 'an administrator cannot remove itself' USING ERRCODE = '22023';
    END IF;

    UPDATE public.users seat SET tenant_id = NULL, role = 'member'
    WHERE seat.id = p_user_id
      AND seat.tenant_id = p_tenant_id
      AND seat.role IN ('member', 'tenant_admin')
    RETURNING seat.email INTO v_email;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'member account is unavailable' USING ERRCODE = 'P0002';
    END IF;

    RETURN v_email;
END;
$$;

CREATE FUNCTION public.admin_delete_account(p_actor_user_id uuid, p_user_id uuid)
RETURNS text
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog
AS $$
DECLARE v_email text;
BEGIN
    IF p_user_id IS NULL THEN
        RAISE EXCEPTION 'target account is required' USING ERRCODE = '22023';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM public.users actor
        WHERE actor.id = p_actor_user_id AND actor.tenant_id IS NULL AND actor.role = 'platform_admin'
    ) THEN
        RAISE EXCEPTION 'platform administrator role is required' USING ERRCODE = '42501';
    END IF;
    IF p_actor_user_id = p_user_id THEN
        RAISE EXCEPTION 'an administrator cannot delete itself' USING ERRCODE = '22023';
    END IF;

    -- Platform administrators are created by the operator command only, so
    -- they are never deletable from the web surface. Sessions cascade.
    DELETE FROM public.users seat
    WHERE seat.id = p_user_id AND seat.role IN ('member', 'tenant_admin')
    RETURNING seat.email INTO v_email;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'member account is unavailable' USING ERRCODE = 'P0002';
    END IF;

    RETURN v_email;
END;
$$;

ALTER FUNCTION public.tenant_remove_member(uuid, uuid, uuid) OWNER TO namo_signup_definer;
ALTER FUNCTION public.admin_delete_account(uuid, uuid) OWNER TO namo_signup_definer;

REVOKE ALL ON FUNCTION public.tenant_remove_member(uuid, uuid, uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.admin_delete_account(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.tenant_remove_member(uuid, uuid, uuid) TO namo_runtime;
GRANT EXECUTE ON FUNCTION public.admin_delete_account(uuid, uuid) TO namo_runtime;
