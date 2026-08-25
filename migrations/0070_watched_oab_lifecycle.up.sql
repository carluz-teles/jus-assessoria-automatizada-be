-- watched_oab gains a toggle lifecycle: adding an OAB to an already-active DJEN
-- integration used to be invisible to the backfill (only the first activation ever
-- fired it). enabled lets the Termos screen turn OFF future capture for one OAB
-- while keeping everything already captured fully visible; disabled_at/catch_up_since
-- carry the gap so re-enabling can request a catch-up sync scoped to the downtime
-- instead of either silence or a full historical re-scan.
ALTER TABLE watched_oab
  ADD COLUMN enabled        boolean     NOT NULL DEFAULT true,
  ADD COLUMN disabled_at    timestamptz,
  ADD COLUMN catch_up_since timestamptz;

-- Partial index: the national match (match.sql) and the backfill/toggle paths only
-- ever care about ENABLED watches; a disabled OAB should not match new publications.
CREATE INDEX watched_oab_enabled_idx ON watched_oab (integration_id) WHERE enabled = true;
