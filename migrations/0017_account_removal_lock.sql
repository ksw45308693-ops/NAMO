-- Keep the applied migration immutable. Lock actor and target in UUID order so
-- concurrent administrators cannot both act on authority revoked by the other.
CREATE OR REPLACE FUNCTION public.tenant_remove_member(
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

    PERFORM seat.id FROM public.users seat
    WHERE seat.tenant_id = p_tenant_id
      AND seat.id IN (p_actor_user_id, p_user_id)
    ORDER BY seat.id FOR UPDATE;

    IF NOT EXISTS (
        SELECT 1 FROM public.users actor
        WHERE actor.id = p_actor_user_id AND actor.tenant_id = p_tenant_id AND actor.role = 'tenant_admin'
    ) THEN
        RAISE EXCEPTION 'company administrator role is required' USING ERRCODE = '42501';
    END IF;
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

ALTER FUNCTION public.tenant_remove_member(uuid, uuid, uuid) OWNER TO namo_signup_definer;
REVOKE ALL ON FUNCTION public.tenant_remove_member(uuid, uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.tenant_remove_member(uuid, uuid, uuid) TO namo_runtime;
