-- Persist the eproc EVENT date (infraEventoData) per document. Only origin=COURT
-- documents fetched from the autos carry it; human UPLOADs leave it NULL. The FE shows
-- it apart from the (now event-description-based) name to disambiguate same-named docs
-- (the many "Certidão"). Nullable, no backfill — docs already downloaded keep NULL until
-- a re-fetch.
ALTER TABLE document ADD COLUMN court_event_date timestamptz;
