CREATE TYPE entry_level AS ENUM ('suggestion', 'error');

ALTER TABLE entry
      ADD COLUMN level entry_level NOT NULL DEFAULT 'error',
      ADD COLUMN data jsonb;

---- create above / drop below ----

ALTER TABLE entry
      DROP COLUMN level,
      DROP COLUMN data;

DROP TYPE entry_level;
