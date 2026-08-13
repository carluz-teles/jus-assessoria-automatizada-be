-- 0029_court_case_assigned_user.up.sql — "responsável do processo" at case level.
-- One user is responsible for a lide (court_case), shared across its graus/records; the
-- ERD keeps the assignment on court_case, not court_record, so the header stays stable as
-- degrees come and go. Nullable = unassigned (the default). ON DELETE SET NULL: removing a
-- member (app_user) drops them as responsável rather than blocking the delete or orphaning
-- the case. court_case already carries tenant_id + the tenant_isolation RLS policy (0001);
-- both barriers still apply — this only adds the column.
ALTER TABLE court_case
  ADD COLUMN assigned_user_id uuid REFERENCES app_user(id) ON DELETE SET NULL;
