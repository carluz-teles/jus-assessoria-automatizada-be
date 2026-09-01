-- 0083_v1_deadline_motor (down) — reverts V1 motor de prazos schema.
-- Order: drop child tables first (applied_holiday references calc_memory),
-- then parent tables, then remove deadline V1 columns.

-- ── 6. deadline V1 columns ──────────────────────────────────────────────────
ALTER TABLE deadline
  DROP COLUMN IF EXISTS origem,
  DROP COLUMN IF EXISTS selo,
  DROP COLUMN IF EXISTS confirmacao_exigida,
  DROP COLUMN IF EXISTS providencia,
  DROP COLUMN IF EXISTS confirmado_por,
  DROP COLUMN IF EXISTS confirmado_em;

-- ── 5. deadline_policy ───────────────────────────────────────────────────────
DROP TABLE IF EXISTS deadline_policy;

-- ── 4. deadline_event ────────────────────────────────────────────────────────
DROP TABLE IF EXISTS deadline_event;

-- ── 3. cross_validation ──────────────────────────────────────────────────────
DROP TABLE IF EXISTS cross_validation;

-- ── 2. applied_holiday ───────────────────────────────────────────────────────
DROP TABLE IF EXISTS applied_holiday;

-- ── 1. calc_memory ───────────────────────────────────────────────────────────
DROP TABLE IF EXISTS calc_memory;
