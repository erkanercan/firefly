BEGIN;

ALTER TABLE tokentransfer ALTER COLUMN key DROP NOT NULL;

COMMIT;