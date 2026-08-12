-- 0028_outbox_process_at.up.sql — opt-in future delivery for outbox events.
-- process_at is the ETA a producer MAY set (via events.ScheduledEvent); the relay
-- enqueues the asynq task with ProcessAt(process_at) so it starts SCHEDULED, not
-- pending. NULL (the default) preserves today's immediate-at-birth behavior. The ETA
-- lives only in asynq — the outbox row is still drained the same tick (no polling,
-- no dual-write). The partial index WHERE published_at IS NULL is unaffected.
ALTER TABLE outbox ADD COLUMN process_at timestamptz;
