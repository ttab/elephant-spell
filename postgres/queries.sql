-- name: SetEntry :exec
INSERT INTO entry(
       language, entry, status, description, common_mistakes, level, data,
       updated, updated_by
) VALUES (
       @language, @entry, @status, @description, @common_mistakes, @level, @data,
       @updated, @updated_by
) ON CONFLICT(language, entry) DO
  UPDATE SET
       status = excluded.status,
       description = excluded.description,
       common_mistakes = excluded.common_mistakes,
       level = excluded.level,
       data = excluded.data,
       updated = excluded.updated,
       updated_by = excluded.updated_by;

-- name: GetEntry :one
SELECT language, entry, status, description, common_mistakes, level, data,
       updated, updated_by
FROM entry
WHERE language = @language AND entry = @entry;

-- name: DeleteEntry :exec
DELETE FROM entry
WHERE language = @language AND entry = @entry;

-- name: ListEntries :many
SELECT language, entry, status, description, common_mistakes, level, data,
       updated, updated_by
FROM entry
WHERE
        (sqlc.narg('language')::text IS NULL OR language = @language)
        AND (sqlc.narg('pattern')::text IS NULL OR entry LIKE @pattern)
        AND (sqlc.narg('status')::text IS NULL OR status = @status)
LIMIT sqlc.arg('limit')::bigint OFFSET sqlc.arg('offset')::bigint;

-- name: ListDictionaries :many
SELECT language, COUNT(*) AS entries
FROM entry
GROUP BY language;

-- name: Notify :exec
SELECT pg_notify(@channel::text, @message::text);
