-- Revert 0006: rename the intimation table back to notification. The RLS policy
-- and the deadline FK follow the table automatically, as on the way up.
ALTER TABLE intimation RENAME TO notification;
