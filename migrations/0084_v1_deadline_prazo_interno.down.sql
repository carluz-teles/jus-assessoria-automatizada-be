-- 0084_v1_deadline_prazo_interno (down) — drops the persisted internal buffer column.

ALTER TABLE deadline DROP COLUMN prazo_interno;
