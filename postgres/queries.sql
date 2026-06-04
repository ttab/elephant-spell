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
ORDER BY language, entry
LIMIT sqlc.arg('limit')::bigint OFFSET sqlc.arg('offset')::bigint;

-- name: ListDictionaries :many
SELECT language, COUNT(*) AS entries
FROM entry
GROUP BY language;

-- LockEventlog takes an exclusive write lock on the eventlog table. Writers
-- must call this before inserting an event so that event commits are
-- serialised: a writer holds the lock until commit, so the next writer cannot
-- draw its id until the previous event is visible. This keeps commit order
-- equal to id order, which the log poller relies on to never skip an event.
-- name: LockEventlog :exec
LOCK TABLE eventlog IN EXCLUSIVE MODE;

-- name: InsertEvent :one
INSERT INTO eventlog(language, entry, deleted)
VALUES (@language, @entry, @deleted)
RETURNING id;

-- name: ReadEventlog :many
SELECT id, language, entry, deleted, created
FROM eventlog
WHERE id > @after
ORDER BY id
LIMIT sqlc.arg('limit')::bigint;

-- name: GetLastEventID :one
SELECT COALESCE(MAX(id), 0)::bigint AS id FROM eventlog;

-- PruneEventlog deletes events older than the cutoff. The eventlog is only a
-- delivery buffer for live consumers, not an audit trail: a restarting replica
-- reloads full state and resumes from the latest id, so events past the
-- consumer lag window are safe to drop.
-- name: PruneEventlog :execrows
DELETE FROM eventlog WHERE created < @before;
