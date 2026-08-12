-- internal/checks' own Checker registry, internal/api/public/checks.go's
-- validCheckTypes allowlist, and this Postgres enum are three independent
-- places that must all list the same check types by hand. "upgrade" and
-- "vless" were registered as Checkers and (as of the previous commit)
-- added to validCheckTypes, but this enum was never extended - every
-- POST /api/v1/checks with type=upgrade or type=vless failed at the DB
-- layer with "invalid input value for enum check_type", after already
-- passing the Go-level validation.
ALTER TYPE check_type ADD VALUE 'upgrade';
ALTER TYPE check_type ADD VALUE 'vless';
