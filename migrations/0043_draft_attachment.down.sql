-- Rollback: drop the join table (policy, indexes and rows all cascade).
DROP TABLE IF EXISTS draft_attachment;
