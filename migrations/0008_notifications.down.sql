-- 0008_notifications.down.sql — drops the delivery table first (it FKs the
-- notification), then the notification table (RLS policies and the tenant_id
-- indexes go with them).

DROP TABLE notification_delivery;
DROP TABLE notification;
