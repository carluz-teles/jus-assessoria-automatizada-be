-- 0084_v1_deadline_prazo_interno — persists the internal safety buffer (2 dias úteis
-- before end_date) as a real column instead of the in-memory placeholder
-- (read.go used to stamp prazo_interno = end_date on every read). Recomputed at the
-- same 4 points end_date already is: birth (OnIntimationObserved), confirm, adjust and
-- the aceita_declarado apuração.
-- Reference: docs/design-motor-de-prazos-v1.md

ALTER TABLE deadline ADD COLUMN prazo_interno date;
