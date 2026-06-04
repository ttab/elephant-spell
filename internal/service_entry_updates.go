package internal

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/ttab/elephant-spell/postgres"
)

// eventBatchSize is how many events a single drain iteration reads per
// round-trip. A drain keeps reading batches until the log is exhausted, so the
// whole catch-up counts as a single FanOut.Polled call regardless of backlog.
const eventBatchSize = 500

// preloadEntries loads every current custom entry into its language's
// spellchecker. It is the startup baseline for entries that predate the
// eventlog; incremental changes are then applied from the log.
func (a *Application) preloadEntries(ctx context.Context) error {
	var (
		limit  int64 = 200
		offset int64
	)

	for {
		rows, err := a.q.ListEntries(ctx, postgres.ListEntriesParams{
			Limit:  limit,
			Offset: offset,
		})
		if err != nil {
			return fmt.Errorf("list entries: %w", err)
		}

		if len(rows) == 0 {
			return nil
		}

		for _, row := range rows {
			spell, ok := a.languages[row.Language]
			if !ok {
				continue
			}

			spell.AddPhrase(entryAsPhrase(row))
		}

		offset += limit
	}
}

// drainEventlog applies every event after the cursor to the spellcheckers and
// returns the number of events applied together with the advanced cursor. The
// log is the single source of truth for incremental changes, so this both
// handles real-time notifications and serves as the fallback poll; the caller
// reports the count to the FanOut via Polled.
func (a *Application) drainEventlog(
	ctx context.Context, after int64,
) (int, int64, error) {
	var applied int

	cursor := after

	for {
		events, err := a.q.ReadEventlog(ctx, postgres.ReadEventlogParams{
			After: cursor,
			Limit: eventBatchSize,
		})
		if err != nil {
			return applied, cursor, fmt.Errorf("read eventlog: %w", err)
		}

		if len(events) == 0 {
			return applied, cursor, nil
		}

		for _, e := range events {
			err := a.applyEvent(ctx, e)
			if err != nil {
				return applied, cursor, fmt.Errorf(
					"apply event %d: %w", e.ID, err)
			}

			cursor = e.ID
		}

		applied += len(events)

		if len(events) < eventBatchSize {
			return applied, cursor, nil
		}
	}
}

// applyEvent brings the relevant spellchecker in line with a single eventlog
// entry. Deletes remove the phrase; for upserts the current entry is read and
// added — and if it has since been removed, the phrase is dropped too, so
// replaying events is idempotent regardless of ordering.
func (a *Application) applyEvent(
	ctx context.Context, e postgres.Eventlog,
) error {
	spell, ok := a.languages[e.Language]
	if !ok {
		return nil
	}

	if e.Deleted {
		spell.RemovePhrase(e.Entry)

		return nil
	}

	entry, err := a.q.GetEntry(ctx, postgres.GetEntryParams{
		Language: e.Language,
		Entry:    e.Entry,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		spell.RemovePhrase(e.Entry)

		return nil
	} else if err != nil {
		return fmt.Errorf("read entry from database: %w", err)
	}

	spell.AddPhrase(entryAsPhrase(entry))

	return nil
}

func entryAsPhrase(e postgres.Entry) Phrase {
	var (
		forms         map[string]string
		caseSensitive bool
	)

	if e.Data != nil {
		forms = e.Data.Forms
		caseSensitive = e.Data.CaseSensitive
	}

	return Phrase{
		Text:           e.Entry,
		Description:    e.Description,
		CommonMistakes: e.CommonMistakes,
		Level:          e.Level,
		Forms:          forms,
		Status:         e.Status,
		CaseSensitive:  caseSensitive,
	}
}
