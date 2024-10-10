package internal

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/ttab/elephant-spell/postgres"
)

type phrase struct {
	Text        string
	Description string
}

func (a *Application) preloadEntries(ctx context.Context) error {
	a.m.Lock()
	defer a.m.Unlock()

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
			checker, ok := a.checkers[row.Language]
			if !ok {
				continue
			}

			trie, ok := a.phrases[row.Language]
			if !ok {
				continue
			}

			p := phrase{
				Text:        row.Entry,
				Description: row.Description,
			}

			trie.Put(row.Entry, &p)
			checker.Add(row.Entry)

			for _, cm := range row.CommonMistakes {
				trie.Put(cm, &p)
			}
		}

		offset += limit
	}
}

func (a *Application) handleEntryUpdate(
	ctx context.Context, n EntryUpdateNotification,
) error {
	a.m.Lock()
	defer a.m.Unlock()

	checker, ok := a.checkers[n.Language]
	if !ok {
		return nil
	}

	trie, ok := a.phrases[n.Language]
	if !ok {
		return nil
	}

	if n.Deleted {
		checker.Remove(n.Text)
		trie.Delete(n.Text)

		return nil
	}

	entry, err := a.q.GetEntry(ctx, postgres.GetEntryParams{
		Language: n.Language,
		Entry:    n.Text,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		checker.Remove(n.Text)
		trie.Delete(n.Text)

		return nil
	} else if err != nil {
		return fmt.Errorf("read entry from database: %w", err)
	}

	p := phrase{
		Text:        n.Text,
		Description: entry.Description,
	}

	trie.Put(n.Text, &p)
	checker.Add(n.Text)

	for _, cm := range entry.CommonMistakes {
		trie.Put(cm, &p)
	}

	return nil
}
