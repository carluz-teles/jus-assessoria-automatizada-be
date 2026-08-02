-- 0011_tenant_email.down.sql — drops the tenant email column.

ALTER TABLE tenant DROP COLUMN email;
