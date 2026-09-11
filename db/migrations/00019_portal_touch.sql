-- +goose Up
-- Fix latent bug: portal session UPDATE portal_links SET last_used_at ran
-- under app.workspace_id='' so tenant RLS saw zero rows — silent no-op since
-- the first portal PR. last_used_at (the §588 portal-open-rate signal) was
-- never recorded. Machine path per ADR-0007: SECURITY DEFINER touch fn.

CREATE OR REPLACE FUNCTION portal_link_touch(p_token_hash text)
RETURNS void
LANGUAGE sql
VOLATILE
SECURITY DEFINER
SET search_path = public
AS $$
    UPDATE portal_links
    SET last_used_at = now(), updated_at = now()
    WHERE token_hash = p_token_hash
      AND status = 'active'
      AND deleted_at IS NULL
$$;

GRANT EXECUTE ON FUNCTION portal_link_touch(text) TO openlane_app;

-- +goose Down
REVOKE EXECUTE ON FUNCTION portal_link_touch(text) FROM openlane_app;
DROP FUNCTION IF EXISTS portal_link_touch(text);
