ALTER TABLE entry
      ADD COLUMN updated timestamptz NOT NULL DEFAULT now(),
      ADD COLUMN updated_by text NOT NULL DEFAULT '';

---- create above / drop below ----

ALTER TABLE entry
      DROP COLUMN updated,
      DROP COLUMN updated_by;
