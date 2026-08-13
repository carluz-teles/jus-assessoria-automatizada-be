-- Down of 0032_party_schema. Drop in reverse dependency order: party_counsel
-- references party (ON DELETE CASCADE), so it goes first. DROP TABLE removes the
-- table's RLS policies and indexes with it.
DROP TABLE IF EXISTS party_counsel;
DROP TABLE IF EXISTS party;
