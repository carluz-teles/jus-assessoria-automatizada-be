-- 0090 down: remove as colunas title/description de action_item.
ALTER TABLE action_item
    DROP COLUMN IF EXISTS description,
    DROP COLUMN IF EXISTS title;
