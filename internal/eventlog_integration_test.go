package internal

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ttab/elephant-spell/postgres"
	"github.com/ttab/elephant-spell/schema"
	"github.com/ttab/eltest"
)

func TestMain(m *testing.M) {
	code := m.Run()

	if err := eltest.PurgeBackingServices(); err != nil {
		log.Printf("purge backing services: %v", err)
	}

	os.Exit(code)
}

// TestEventlogPruning verifies that the retention cutoff drops events older
// than the window while keeping recent ones. The end-to-end eventlog sync flow
// is covered over the wire in internal/integration.
func TestEventlogPruning(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pg := eltest.NewPostgres(t, eltest.Postgres17_6)
	env := pg.Database(t, "spell", schema.Migrations, true)

	pool, err := pgxpool.New(ctx, env.PostgresURI)
	eltest.Must(t, err, "create connection pool")
	t.Cleanup(pool.Close)

	q := postgres.New(pool)

	// One event well past the retention window, one fresh.
	insert := func(entry string, created time.Time) {
		_, err := pool.Exec(ctx,
			`INSERT INTO eventlog(language, entry, deleted, created)
			 VALUES ('sv-se', $1, false, $2)`,
			entry, pgtype.Timestamptz{Time: created, Valid: true})
		eltest.Must(t, err, "insert event")
	}

	insert("stale", time.Now().Add(-2*eventlogRetention))
	insert("fresh", time.Now())

	removed, err := q.PruneEventlog(ctx, pgtype.Timestamptz{
		Time:  time.Now().Add(-eventlogRetention),
		Valid: true,
	})
	eltest.Must(t, err, "prune eventlog")

	if removed != 1 {
		t.Fatalf("pruned %d events, want 1", removed)
	}

	remaining, err := q.ReadEventlog(ctx, postgres.ReadEventlogParams{
		After: 0,
		Limit: 10,
	})
	eltest.Must(t, err, "read eventlog")

	if len(remaining) != 1 || remaining[0].Entry != "fresh" {
		t.Fatalf("expected only the fresh event to remain, got %+v", remaining)
	}

	t.Log("retention pruning kept the fresh event and dropped the stale one")
}
