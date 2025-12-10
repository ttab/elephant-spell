package internal

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/ttab/elephant-spell/postgres"
)

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

func (a *Application) handleEntryUpdate(
	ctx context.Context, n EntryUpdateNotification,
) error {
	spell, ok := a.languages[n.Language]
	if !ok {
		return nil
	}

	if n.Deleted {
		spell.RemovePhrase(n.Text)

		return nil
	}

	entry, err := a.q.GetEntry(ctx, postgres.GetEntryParams{
		Language: n.Language,
		Entry:    n.Text,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		spell.RemovePhrase(n.Text)

		return nil
	} else if err != nil {
		return fmt.Errorf("read entry from database: %w", err)
	}

	spell.AddPhrase(entryAsPhrase(entry))

	return nil
}

func entryAsPhrase(e postgres.Entry) Phrase {
	var forms map[string]string

	if e.Data != nil {
		forms = e.Data.Forms
	}

	return Phrase{
		Text:           e.Entry,
		Description:    e.Description,
		CommonMistakes: e.CommonMistakes,
		Level:          e.Level,
		Forms:          forms,
	}
}
