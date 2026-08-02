-- 0010_tenant_phone.down.sql — drops the tenant phone column.

ALTER TABLE tenant DROP COLUMN phone;
