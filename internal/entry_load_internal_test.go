package internal

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ttab/elephant-spell/postgres"
	"github.com/ttab/elephant-spell/schema"
	"github.com/ttab/eltest"
)

// TestLoadSkipsEntriesOverThePhraseLimit checks that a stored entry with a
// phrase longer than the window, written before the limit was enforced, is
// skipped with a log line by the preload and by the eventlog, without failing
// either or holding back the other entries.
func TestLoadSkipsEntriesOverThePhraseLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pg := eltest.NewPostgres(t, eltest.Postgres17_6)
	env := pg.Database(t, "spell", schema.Migrations, true)

	pool, err := pgxpool.New(ctx, env.PostgresURI)
	eltest.Must(t, err, "create connection pool")
	t.Cleanup(pool.Close)

	q := postgres.New(pool)

	set := func(entry string, mistakes []string) {
		err := q.SetEntry(ctx, postgres.SetEntryParams{
			Language:       "sv-se",
			Entry:          entry,
			Status:         "accepted",
			CommonMistakes: mistakes,
			Level:          postgres.EntryLevelError,
			Data:           &postgres.EntryData{},
			Updated:        pgtype.Timestamptz{Time: time.Now(), Valid: true},
			UpdatedBy:      "test",
		})
		eltest.Must(t, err, "write entry %q", entry)
	}

	set("till exempel", []string{"tex"})
	set("ett två tre fyra fem sex", nil)
	set("Qwzx", []string{"ett två tre fyra fem sex sju"})

	var logs bytes.Buffer

	check := newCasingCheck(t)

	app := &Application{
		logger:    slog.New(slog.NewTextHandler(&logs, nil)),
		q:         q,
		languages: map[string]*Spellcheck{"sv-se": check},

		maxExpansions: DefaultMaxExpansions,
	}

	err = app.preloadEntries(ctx)
	eltest.Must(t, err, "preload entries")

	got := misspelledTexts(t, check, "Det var tex så.", true)
	wantSuggestions(t, got, "tex", "till exempel")

	for _, skipped := range []string{"ett två tre fyra fem sex", "Qwzx"} {
		if _, ok := check.entries[skipped]; ok {
			t.Errorf("entry %q loaded, want it skipped", skipped)
		}

		if !strings.Contains(logs.String(), skipped) {
			t.Errorf("no log line for skipped entry %q in:\n%s",
				skipped, logs.String())
		}
	}

	// An update through the eventlog that takes a loaded entry over the
	// limit removes it rather than failing the drain.
	set("till exempel", []string{"tex", "a b c d e f"})

	err = app.applyEntryEvent(ctx, check, postgres.Eventlog{
		Language: "sv-se", Entry: "till exempel", Kind: eventKindEntry,
	})
	eltest.Must(t, err, "apply entry event")

	if _, ok := check.entries["till exempel"]; ok {
		t.Error("entry over the limit still loaded after the event")
	}
}

// TestLoadSkipsEntriesOverTheExpansionLimit checks that a stored entry whose
// common mistakes expand to more strings than the limit, written before the
// limit was enforced or under a higher one, is skipped with a log line by the
// preload and by the eventlog instead of being expanded.
func TestLoadSkipsEntriesOverTheExpansionLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pg := eltest.NewPostgres(t, eltest.Postgres17_6)
	env := pg.Database(t, "spell", schema.Migrations, true)

	pool, err := pgxpool.New(ctx, env.PostgresURI)
	eltest.Must(t, err, "create connection pool")
	t.Cleanup(pool.Close)

	q := postgres.New(pool)

	set := func(entry string, mistakes []string) {
		err := q.SetEntry(ctx, postgres.SetEntryParams{
			Language:       "sv-se",
			Entry:          entry,
			Status:         "accepted",
			CommonMistakes: mistakes,
			Level:          postgres.EntryLevelError,
			Data:           &postgres.EntryData{},
			Updated:        pgtype.Timestamptz{Time: time.Now(), Valid: true},
			UpdatedBy:      "test",
		})
		eltest.Must(t, err, "write entry %q", entry)
	}

	set("till exempel", []string{"tex"})
	set("Qwzx", []string{strings.Repeat("{a|b}", 100)})
	set("Zqwx", []string{patternOf("zq", 100), patternOf("zr", 101)})

	var logs bytes.Buffer

	check := newCasingCheck(t)

	app := &Application{
		logger:    slog.New(slog.NewTextHandler(&logs, nil)),
		q:         q,
		languages: map[string]*Spellcheck{"sv-se": check},

		maxExpansions: DefaultMaxExpansions,
	}

	err = app.preloadEntries(ctx)
	eltest.Must(t, err, "preload entries")

	got := misspelledTexts(t, check, "Det var tex så.", true)
	wantSuggestions(t, got, "tex", "till exempel")

	for _, skipped := range []string{"Qwzx", "Zqwx"} {
		if _, ok := check.entries[skipped]; ok {
			t.Errorf("entry %q loaded, want it skipped", skipped)
		}

		if !strings.Contains(logs.String(), "entry="+skipped) {
			t.Errorf("no log line for skipped entry %q in:\n%s",
				skipped, logs.String())
		}
	}

	// An update through the eventlog that takes a loaded entry over the
	// limit removes it rather than failing the drain.
	set("till exempel", []string{"tex", patternOf("x", 200)})

	err = app.applyEntryEvent(ctx, check, postgres.Eventlog{
		Language: "sv-se", Entry: "till exempel", Kind: eventKindEntry,
	})
	eltest.Must(t, err, "apply entry event")

	if _, ok := check.entries["till exempel"]; ok {
		t.Error("entry over the limit still loaded after the event")
	}
}
