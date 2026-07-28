-- 0001_init.up.sql — initial v0 schema.
-- Faithful translation of docs/erd-modelo-de-dados.md (source of truth for the schema).
-- Identifiers in English; enums as text + CHECK-on-app; tenant_id on every user table.
-- Tables are declared in FK dependency order.

CREATE EXTENSION IF NOT EXISTS vector;     -- pgvector, for chunk.embedding
CREATE EXTENSION IF NOT EXISTS pgcrypto;   -- gen_random_uuid()

-- ============================================================
-- Identity
-- ============================================================

-- tenant — mirrors the Clerk Organization (the escritório). Has no tenant_id of
-- its own: it IS the tenant, so no RLS policy applies to it.
CREATE TABLE tenant (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  clerk_org_id text NOT NULL UNIQUE,       -- bridge to Clerk
  name         text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);

-- app_user — mirrors the Clerk User (1 user = 1 escritório).
CREATE TABLE app_user (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  clerk_user_id text NOT NULL UNIQUE,
  tenant_id     uuid NOT NULL REFERENCES tenant(id),
  email         text NOT NULL,
  name          text,
  role          text NOT NULL DEFAULT 'LAWYER',   -- ADMIN | LAWYER
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON app_user (tenant_id);

-- ============================================================
-- Acquisition
-- ============================================================

-- integration — a tenant's subscription to a data source (not a court).
CREATE TABLE integration (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id      uuid NOT NULL REFERENCES tenant(id),
  source         text NOT NULL,           -- DATAJUD | DJEN | UPLOAD (v1+: MNI, ESAJ...)
  scope          jsonb NOT NULL,          -- { oab: [], taxId: [] }
  credential_ref text,                    -- pointer to vault; null in v0
  status         text NOT NULL DEFAULT 'ACTIVE',  -- ACTIVE|DEGRADED|AUTH_FAILED|DISABLED
  created_at     timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, source)
);

-- ============================================================
-- Consolidation
-- ============================================================

-- court_case — the lide, what *we* know. Named court_case because `case` is
-- reserved in SQL. primary_court_record_id is a UI hint (no FK, on purpose).
CREATE TABLE court_case (
  id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id               uuid NOT NULL REFERENCES tenant(id),
  label                   text,
  primary_court_record_id uuid,                              -- UI hint, not truth
  merged_into_id          uuid REFERENCES court_case(id),    -- merge never deletes [v1+]
  created_at              timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON court_case (tenant_id);

-- court_record — what *the court* knows, keyed by (cnj_number, degree, court).
CREATE TABLE court_record (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id      uuid NOT NULL REFERENCES tenant(id),
  case_id        uuid NOT NULL REFERENCES court_case(id),
  cnj_number     text NOT NULL,           -- unmasked
  degree         text NOT NULL,           -- G1|G2|JE|SUPERIOR|UNKNOWN
  court          text NOT NULL,
  class          text,
  subject        text,
  claim_value    numeric(15,2),
  secrecy        text NOT NULL DEFAULT 'PUBLIC',    -- PUBLIC|RESTRICTED|SECRET
  lifecycle      text NOT NULL DEFAULT 'ACTIVE',    -- ACTIVE|SUSPENDED|ARCHIVED|SUPERSEDED
  completeness   real NOT NULL DEFAULT 0,
  sync_policy    jsonb NOT NULL DEFAULT '{}',
  next_sync_at   timestamptz,
  last_synced_at jsonb NOT NULL DEFAULT '{}',       -- Record<Source, Date>
  UNIQUE (tenant_id, cnj_number, degree)
);
CREATE INDEX ON court_record (next_sync_at) WHERE lifecycle = 'ACTIVE';  -- the hottest query (scheduler)
CREATE INDEX ON court_record (tenant_id, case_id);

-- sync_run — auditable record of each sync execution. References court_record,
-- so it is declared after it (null court_record_id on OAB discovery).
CREATE TABLE sync_run (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL REFERENCES tenant(id),
  court_record_id   uuid REFERENCES court_record(id),   -- null on OAB discovery
  integration_id    uuid NOT NULL REFERENCES integration(id),
  connector_id      text NOT NULL,
  connector_version text NOT NULL,
  started_at        timestamptz NOT NULL,
  finished_at       timestamptz,
  status            text NOT NULL,        -- RUNNING | OK | FAILED
  items_new         int NOT NULL DEFAULT 0,
  items_deduped     int NOT NULL DEFAULT 0,
  raw_payload_refs  jsonb NOT NULL DEFAULT '[]',
  error             jsonb
);
CREATE INDEX ON sync_run (tenant_id, started_at);

-- docket_entry — the chronological log of andamentos. Fastest-growing table.
-- The two dates carry the most weight in the schema.
CREATE TABLE docket_entry (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  court_record_id uuid NOT NULL REFERENCES court_record(id),
  hash            text NOT NULL,          -- sha256(court|number|degree|date|text)
  occurred_at     timestamptz NOT NULL,   -- when the court acted -> domain math
  observed_at     timestamptz NOT NULL,   -- when we discovered -> ordering & idempotency
  source          text NOT NULL,
  fidelity        int NOT NULL,
  tpu_code        int,                    -- Tabela Processual Unificada code
  complements     jsonb,
  text            text NOT NULL,
  retracted_at    timestamptz,            -- never delete, only mark
  UNIQUE (court_record_id, hash)
);
CREATE INDEX ON docket_entry (court_record_id, occurred_at);
CREATE INDEX ON docket_entry (tpu_code);

-- notification — intimação that opens a prazo. Dedup within the case scope.
CREATE TABLE notification (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL REFERENCES tenant(id),
  case_id           uuid NOT NULL REFERENCES court_case(id),
  court_record_id   uuid NOT NULL REFERENCES court_record(id),
  hash              text NOT NULL,
  made_available_at date NOT NULL,        -- disponibilização (what the API delivers)
  published_at      date NOT NULL,        -- derived: next business day
  deadline_start_at date NOT NULL,        -- derived: first business day after publication
  content           text NOT NULL,        -- teor
  source            text NOT NULL,
  recipients        jsonb NOT NULL DEFAULT '[]',
  UNIQUE (tenant_id, case_id, hash)
);

-- case_link — links two court_records of the same lide, records *why*.
CREATE TABLE case_link (
  id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id            uuid NOT NULL REFERENCES tenant(id),
  from_court_record_id uuid NOT NULL REFERENCES court_record(id),
  to_court_record_id   uuid NOT NULL REFERENCES court_record(id),
  type                 text NOT NULL,    -- APPEAL_OF|SUPERIOR_APPEAL_OF|REDISTRIBUTED_FROM|INCIDENT_OF|LETTER_ROGATORY_OF|RELATED_TO
  confidence           text NOT NULL,    -- v0: only DETERMINISTIC (v1+: DECLARED, INFERRED)
  source               text,
  evidence             text NOT NULL,
  confirmed_by         uuid,             -- app_user, on human confirmation [v1+]
  confirmed_at         timestamptz,
  UNIQUE (from_court_record_id, to_court_record_id, type)
);

-- ============================================================
-- Documents
-- ============================================================

-- document — a file from the autos or an upload (court_record_id null on upload).
CREATE TABLE document (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL REFERENCES tenant(id),
  court_record_id   uuid REFERENCES court_record(id),   -- null on upload
  document_type     text NOT NULL,
  origin            text NOT NULL,        -- COURT | UPLOAD
  storage_key       text,                 -- null while PENDING
  pages             int,
  has_text_layer    boolean NOT NULL DEFAULT false,     -- decides OCR
  extracted_at      timestamptz,
  extractor_version text,
  created_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON document (court_record_id);

-- chunk — a document slice indexed for retrieval (pgvector). No tenant_id
-- (reached only through document); the similarity index arrives with retrieval [v1].
CREATE TABLE chunk (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  document_id uuid NOT NULL REFERENCES document(id),
  page        int NOT NULL,
  text        text NOT NULL,
  embedding   vector(1536)
);
CREATE INDEX ON chunk (document_id, page);

-- ============================================================
-- Advisory
-- ============================================================

-- draft — minuta. case_id optional (upload review needs no case).
CREATE TABLE draft (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL REFERENCES tenant(id),
  case_id     uuid REFERENCES court_case(id),           -- optional
  piece_type  text NOT NULL,                        -- DEFENSE|COMPLAINT|APPEAL|...
  status      text NOT NULL DEFAULT 'DRAFT',        -- DRAFT|REVIEWED|SIGNED
  saga_state  text NOT NULL DEFAULT 'CREATED',      -- CREATED->EXTRACTING->REVIEWED->SIGNED->FILED->LABELED
  storage_key text NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON draft (tenant_id, status);

-- review — the AI parecer, versioned by model and rules. No tenant_id (via draft).
CREATE TABLE review (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  draft_id      uuid NOT NULL REFERENCES draft(id),
  findings      jsonb NOT NULL,           -- Finding[] with citations
  coverage      jsonb NOT NULL,           -- { verified[], notVerified[] }
  model_version text NOT NULL,
  rules_version text NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON review (draft_id, created_at);

-- petition — a signed, filed minuta. Immutable. No tenant_id (via draft).
CREATE TABLE petition (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  draft_id        uuid NOT NULL UNIQUE REFERENCES draft(id),
  court_record_id uuid NOT NULL REFERENCES court_record(id),
  filed_at        timestamptz NOT NULL,
  receipt         jsonb NOT NULL,
  observed_result text                     -- OK|AMENDMENT|NOT_ADMITTED|UNTIMELY
);
CREATE INDEX ON petition (court_record_id) WHERE observed_result IS NULL;  -- close the loop

-- ============================================================
-- Deadlines
-- ============================================================

-- deadline — countdown derived from a notification, anchored on court_record.
CREATE TABLE deadline (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  court_record_id  uuid NOT NULL REFERENCES court_record(id),
  notification_id  uuid NOT NULL UNIQUE REFERENCES notification(id),
  start_date       date NOT NULL,
  end_date         date NOT NULL,
  days             int NOT NULL,
  counting         text NOT NULL,         -- BUSINESS | CALENDAR
  doubled          boolean NOT NULL DEFAULT false,
  holidays_applied jsonb NOT NULL DEFAULT '[]',   -- auditable
  status           text NOT NULL DEFAULT 'OPEN'   -- OPEN|MET|MISSED
);
CREATE INDEX ON deadline (end_date) WHERE status = 'OPEN';   -- expiry sweep

-- ============================================================
-- Infra
-- ============================================================

-- outbox — transactional outbox. bigserial id = publication order. No tenant_id.
CREATE TABLE outbox (
  id              bigserial PRIMARY KEY,   -- publication order
  aggregate_type  text NOT NULL,
  aggregate_id    uuid NOT NULL,           -- partition key for ordering
  type            text NOT NULL,           -- dotted id: domain.fact
  payload         jsonb NOT NULL,
  idempotency_key text,
  trace_context   text,                    -- W3C traceparent — distributed hop
  created_at      timestamptz NOT NULL DEFAULT now(),
  published_at    timestamptz
);
CREATE INDEX ON outbox (id) WHERE published_at IS NULL;  -- the relay reads this

-- processed_event — consumer idempotency, keyed by (consumer, event_id).
CREATE TABLE processed_event (
  consumer     text NOT NULL,
  event_id     text NOT NULL,
  processed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (consumer, event_id)
);

-- backfill_job — onboarding saga state (choreographed). Counters.
CREATE TABLE backfill_job (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id      uuid NOT NULL REFERENCES tenant(id),
  integration_id uuid NOT NULL REFERENCES integration(id),
  window_from    date NOT NULL,
  window_to      date NOT NULL,
  total_slices   int NOT NULL,
  slices_ok      int NOT NULL DEFAULT 0,
  slices_error   int NOT NULL DEFAULT 0,
  status         text NOT NULL DEFAULT 'RUNNING',  -- RUNNING|COMPLETED|PARTIAL
  created_at     timestamptz NOT NULL DEFAULT now()
);

-- ============================================================
-- Row-Level Security — barrier 2 of tenant isolation (docs/erd-backend.md 4d.4).
-- One policy per table that HAS a tenant_id column. current_setting(..., true)
-- uses missing_ok=true so non-tenant contexts (migrations, workers) do not error.
-- ============================================================

ALTER TABLE app_user ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON app_user
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

ALTER TABLE integration ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON integration
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

ALTER TABLE court_case ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON court_case
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

ALTER TABLE court_record ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON court_record
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

ALTER TABLE sync_run ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON sync_run
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

ALTER TABLE notification ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON notification
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

ALTER TABLE case_link ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON case_link
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

ALTER TABLE document ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON document
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

ALTER TABLE draft ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON draft
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

ALTER TABLE backfill_job ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON backfill_job
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
