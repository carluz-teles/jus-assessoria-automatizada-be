-- Reverte 0057: reintroduz conductor_user_id + reviewer_user_id a partir de
-- assignee_user_id. Perda semântica: quem era originalmente "condutor" vs "revisor"
-- não é recuperado — todo mundo cai em conductor_user_id.
ALTER TABLE intimation RENAME COLUMN assignee_user_id TO conductor_user_id;
ALTER INDEX intimation_assignee_user_id_idx RENAME TO intimation_conductor_user_id_idx;

ALTER TABLE intimation
  ADD COLUMN reviewer_user_id uuid REFERENCES app_user(id);
CREATE INDEX ON intimation (reviewer_user_id) WHERE reviewer_user_id IS NOT NULL;
