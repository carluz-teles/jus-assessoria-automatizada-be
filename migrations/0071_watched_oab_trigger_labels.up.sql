-- watched_oab lifecycle (0070) fixed WHAT happens on add/disable/reenable; this
-- migration adds WHY, so the Capturas/Termos screens can show which OAB — and
-- which action — caused a given ingestion, instead of an anonymous "carga
-- inicial". trigger_reason is nullable everywhere: the daily firehose fan-out,
-- the DATAJUD enrichment pass and the pre-0070 backfill/sync_run rows have no
-- OAB to attribute (NULL stays NULL, no backfill needed).

ALTER TABLE backfill_job
  ADD COLUMN trigger_reason text CHECK (trigger_reason IN ('OAB_ADDED')),
  ADD COLUMN trigger_oabs text[];

ALTER TABLE sync_run
  ADD COLUMN trigger_reason text CHECK (trigger_reason IN ('OAB_REENABLED')),
  ADD COLUMN trigger_oab text;

ALTER TABLE watched_oab
  ADD COLUMN last_action text CHECK (last_action IN ('ADDED', 'DISABLED', 'REENABLED')),
  ADD COLUMN last_action_at timestamptz;
