CREATE TABLE rule(
       language text NOT NULL,
       name text NOT NULL,
       status text NOT NULL,
       description text NOT NULL,
       level entry_level NOT NULL DEFAULT 'error',
       pattern text NOT NULL,
       replacement text NOT NULL,
       data jsonb,
       updated timestamptz NOT NULL DEFAULT now(),
       updated_by text NOT NULL DEFAULT '',
       primary key(language, name)
);

ALTER TABLE eventlog ADD COLUMN kind text NOT NULL DEFAULT 'entry';

-- Move rules that were previously stored as dictionary entries (data->'rule')
-- into the dedicated rule table, keeping their guards in the rule data.
INSERT INTO rule(
       language, name, status, description, level, pattern, replacement, data,
       updated, updated_by)
SELECT language, entry, status, description, level,
       COALESCE(data->'rule'->>'pattern', ''),
       COALESCE(data->'rule'->>'replacement', ''),
       NULLIF((data->'rule') - 'pattern' - 'replacement', '{}'::jsonb),
       updated, updated_by
FROM entry
WHERE data ? 'rule';

DELETE FROM entry WHERE data ? 'rule';

---- create above / drop below ----

ALTER TABLE eventlog DROP COLUMN kind;
DROP TABLE rule;
