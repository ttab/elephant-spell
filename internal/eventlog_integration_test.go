package internal

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephant-api/spell"
	"github.com/ttab/elephant-spell/postgres"
	"github.com/ttab/elephant-spell/schema"
	"github.com/ttab/elephantine"
	"github.com/ttab/eltest"
)

func TestMain(m *testing.M) {
	code := m.Run()

	if err := eltest.PurgeBackingServices(); err != nil {
		log.Printf("purge backing services: %v", err)
	}

	os.Exit(code)
}

// TestEventlogIntegration exercises the full real-time sync path against a
// real PostgreSQL instance: a SetEntry/DeleteEntry RPC writes the entry and an
// eventlog row in one transaction and fires a NOTIFY, the Subscriber delivers
// it through the FanOut, and the consumer drains the eventlog into the
// spellchecker. The delete half is the case the previous notification-payload
// design could not recover from a missed event.
func TestEventlogIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pg := eltest.NewPostgres(t, eltest.Postgres17_6)
	env := pg.Database(t, "spell", schema.Migrations, true)

	pool, err := pgxpool.New(ctx, env.PostgresURI)
	eltest.Must(t, err, "create connection pool")
	t.Cleanup(pool.Close)

	logger := elephantine.SetUpLogger("debug", os.Stdout)

	app, err := NewApplication(ctx, Parameters{
		Logger:         logger,
		Database:       pool,
		PubsubDatabase: pool,
		Registerer:     prometheus.NewRegistry(),
		PingInterval:   time.Second,
		PingGrace:      3 * time.Second,
	})
	eltest.Must(t, err, "create application")

	// Start the background sync the same way Run does, but without the HTTP
	// server: the Subscriber owns the LISTEN connection and the entry updater
	// follows the eventlog.
	bgCtx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)

	go func() { _ = app.subscriber.Run(bgCtx) }()
	go func() { _ = app.runEntryUpdater(bgCtx) }()

	// Let the listener register its LISTEN before we publish.
	time.Sleep(time.Second)

	authCtx := elephantine.SetAuthInfo(ctx, &elephantine.AuthInfo{
		Claims: elephantine.JWTClaims{
			RegisteredClaims: jwt.RegisteredClaims{
				Subject: "core://test/spell",
			},
			Scope: ScopeSpellcheckWrite,
		},
	})

	// "Gadaffi" is a registered common mistake for the entry "Gaddafi", so a
	// custom-only check of it is flagged exactly when the entry is loaded.
	const word = "Gadaffi"

	if misspelled(t, authCtx, app, word) {
		t.Fatal("precondition: word should not be flagged before any write")
	}

	_, err = app.SetEntry(authCtx, &spell.SetEntryRequest{
		Entry: &spell.CustomEntry{
			Language:       "sv-se",
			Text:           "Gaddafi",
			Status:         "approved",
			CommonMistakes: []string{word},
			Level:          spell.CorrectionLevel_LEVEL_ERROR,
		},
	})
	eltest.Must(t, err, "set entry")

	waitForFlag(t, authCtx, app, word, true)
	t.Log("upsert propagated through eventlog")

	_, err = app.DeleteEntry(authCtx, &spell.DeleteEntryRequest{
		Language: "sv-se",
		Text:     "Gaddafi",
	})
	eltest.Must(t, err, "delete entry")

	waitForFlag(t, authCtx, app, word, false)
	t.Log("delete propagated through eventlog (gap closed)")
}

// TestEventlogPruning verifies that the retention cutoff drops events older
// than the window while keeping recent ones.
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

// misspelled reports whether a custom-only spellcheck of word flags it.
func misspelled(
	t *testing.T, ctx context.Context, app *Application, word string,
) bool {
	t.Helper()

	res, err := app.Text(ctx, &spell.TextRequest{
		Language:   "sv-se",
		Text:       []string{word},
		CustomOnly: true,
	})
	eltest.Must(t, err, "spellcheck text")

	return len(res.Misspelled) == 1 && len(res.Misspelled[0].Entries) > 0
}

// waitForFlag polls until the flagged state of word matches want, failing the
// test if it does not converge. Propagation is asynchronous (RPC commit →
// NOTIFY → subscriber → FanOut → drain).
func waitForFlag(
	t *testing.T, ctx context.Context, app *Application, word string, want bool,
) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		if misspelled(t, ctx, app, word) == want {
			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("word %q flagged state did not reach %v within timeout", word, want)
}
