-- Mark every dictionary entry whose text contains an upper-case character as
-- case-sensitive. `entry <> lower(entry)` is a Unicode-aware test: it is true
-- whenever some character has a distinct lower-case form (A-Z, Å, Ä, Ö, …).
-- jsonb_set preserves any existing data (forms, guards) and only sets the flag
-- on entries that aren't already case-sensitive.
UPDATE entry
SET data = jsonb_set(COALESCE(data, '{}'::jsonb), '{case_sensitive}', 'true'::jsonb)
WHERE entry <> lower(entry)
  AND COALESCE((data ->> 'case_sensitive')::boolean, false) = false;

---- create above / drop below ----

-- Data backfill: the original case-sensitivity of each entry isn't recoverable
-- (we can't tell which were already case-sensitive before the migration), so
-- the rollback is intentionally a no-op.
