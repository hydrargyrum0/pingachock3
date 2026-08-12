-- Postgres has no ALTER TYPE ... DROP VALUE - the only way back is to
-- recreate the enum without 'upgrade'/'vless' and repoint the one column
-- that uses it. Fails loudly (not silently) if any existing row actually
-- has type='upgrade' or type='vless', since neither value exists in the
-- new enum - that's the correct behavior for a rollback, not a bug.
ALTER TYPE check_type RENAME TO check_type_old;
CREATE TYPE check_type AS ENUM ('ping', 'tcp', 'http', 'dns', 'tls', 'traceroute');
ALTER TABLE checks ALTER COLUMN type TYPE check_type USING type::text::check_type;
DROP TYPE check_type_old;
