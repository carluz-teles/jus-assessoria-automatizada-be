-- match queries (acquisition slice). The join that turns the national publication
-- firehose into per-tenant work: a watched_oab key ∈ a publication's recipient_oabs.
-- Read system-wide (uow.DoSystem sets app.system='on' so the watched_oab RLS opens
-- across tenants); publication is national and unscoped.

-- name: MatchPublicationsByDay :many
-- Every (tenant, matched OAB, publication payload) for the communications made
-- available on a day. One publication watched by two OABs of the same tenant yields
-- two rows; the match use case groups by tenant and dedups the payloads. payload is
-- the raw DJEN item, re-parsed per matched tenant to create its court_record/intimação.
SELECT w.tenant_id, w.oab_key, p.payload
FROM publication p
JOIN watched_oab w ON w.oab_key = ANY (p.recipient_oabs)
WHERE p.made_available_at = $1;
