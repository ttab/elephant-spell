package internal

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/ttab/elephant-spell/postgres"
	"github.com/ttab/elephantine"
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

// preloadRules loads every current rule into its language's spellchecker, the
// startup baseline before the eventlog takes over.
func (a *Application) preloadRules(ctx context.Context) error {
	var (
		limit  int64 = 200
		offset int64
	)

	for {
		rows, err := a.q.ListRules(ctx, postgres.ListRulesParams{
			Limit:  limit,
			Offset: offset,
		})
		if err != nil {
			return fmt.Errorf("list rules: %w", err)
		}

		if len(rows) == 0 {
			return nil
		}

		for _, row := range rows {
			spell, ok := a.languages[row.Language]
			if !ok {
				continue
			}

			err := spell.AddRule(ruleDefFromRule(row))
			if err != nil {
				a.logger.ErrorContext(ctx, "skip invalid rule",
					"rule", row.Name, "language", row.Language,
					elephantine.LogKeyError, err)
			}
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
// entry, routing by kind to the word or rule store. Deletes remove the item;
// for upserts the current row is read and added — and if it has since been
// removed, the item is dropped too, so replaying events is idempotent
// regardless of ordering.
func (a *Application) applyEvent(
	ctx context.Context, e postgres.Eventlog,
) error {
	spell, ok := a.languages[e.Language]
	if !ok {
		return nil
	}

	if e.Kind == eventKindRule {
		return a.applyRuleEvent(ctx, spell, e)
	}

	return a.applyEntryEvent(ctx, spell, e)
}

func (a *Application) applyEntryEvent(
	ctx context.Context, s *Spellcheck, e postgres.Eventlog,
) error {
	if e.Deleted {
		s.RemovePhrase(e.Entry)

		return nil
	}

	entry, err := a.q.GetEntry(ctx, postgres.GetEntryParams{
		Language: e.Language,
		Entry:    e.Entry,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		s.RemovePhrase(e.Entry)

		return nil
	} else if err != nil {
		return fmt.Errorf("read entry from database: %w", err)
	}

	s.AddPhrase(entryAsPhrase(entry))

	return nil
}

func (a *Application) applyRuleEvent(
	ctx context.Context, s *Spellcheck, e postgres.Eventlog,
) error {
	if e.Deleted {
		s.RemoveRule(e.Entry)

		return nil
	}

	row, err := a.q.GetRule(ctx, postgres.GetRuleParams{
		Language: e.Language,
		Name:     e.Entry,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		s.RemoveRule(e.Entry)

		return nil
	} else if err != nil {
		return fmt.Errorf("read rule from database: %w", err)
	}

	// A rule that fails to compile is logged and skipped rather than stalling
	// the eventlog; patterns are validated on write so this is defensive.
	err = s.AddRule(ruleDefFromRule(row))
	if err != nil {
		a.logger.ErrorContext(ctx, "skip invalid rule",
			"rule", row.Name, "language", row.Language,
			elephantine.LogKeyError, err)
	}

	return nil
}

func ruleDefFromRule(r postgres.Rule) RuleDef {
	def := RuleDef{
		Name:        r.Name,
		Pattern:     r.Pattern,
		Replacement: r.Replacement,
		Description: r.Description,
		Level:       r.Level,
		Status:      r.Status,
	}

	if r.Data != nil {
		def.Before = r.Data.Before
		def.After = r.Data.After
		def.NotBefore = r.Data.NotBefore
		def.NotAfter = r.Data.NotAfter
	}

	return def
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
