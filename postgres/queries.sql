-- name: SetEntry :exec
INSERT INTO entry(
       language, entry, status, description, common_mistakes
) VALUES (
       @language, @entry, @status, @description, @common_mistakes
) ON CONFLICT(language, entry) DO
  UPDATE SET
       status = @status,
       description = @description,
       common_mistakes = @common_mistakes;

-- name: GetEntry :one
SELECT language, entry, status, description, common_mistakes
FROM entry
WHERE language = @language AND entry = @entry;

-- name: DeleteEntry :exec
DELETE FROM entry
WHERE language = @language AND entry = @entry;

-- name: ListEntries :many
SELECT language, entry, status, description, common_mistakes
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
