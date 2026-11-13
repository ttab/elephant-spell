package internal

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/ttab/elephant-spell/postgres"
	"github.com/ttab/elephantine"
)

// eventBatchSize is how many events a single drain iteration reads per
// round-trip. A drain keeps reading batches until the log is exhausted, so the
// whole catch-up counts as a single FanOut.Polled call regardless of backlog.
const eventBatchSize = 500

// preloadPageSize is the batch size used when loading the startup baseline.
const preloadPageSize = 200

// preloadAll pages through a list query, applying process to every row, until
// a short page says the source is exhausted. fetch is handed the last row of
// the previous page, nil for the first, and reads the rows that follow it in
// key order. Paging by key rather than by offset is what keeps a concurrent
// delete from shifting an unread row onto a page already read: the next page
// starts after a key, not after a count. It is the shared startup-baseline
// loader for entries and rules. afterPage, when not nil, runs after each
// page has been processed.
func preloadAll[T any](
	ctx context.Context, pageSize int64,
	fetch func(ctx context.Context, after *T, limit int64) ([]T, error),
	process func(row T),
	afterPage func(),
) error {
	var after *T

	for {
		rows, err := fetch(ctx, after, pageSize)
		if err != nil {
			return err
		}

		for _, row := range rows {
			process(row)
		}

		if afterPage != nil {
			afterPage()
		}

		if int64(len(rows)) < pageSize {
			return nil
		}

		after = &rows[len(rows)-1]
	}
}

// preloadEntries loads every current custom entry into its language's
// spellchecker. It is the startup baseline for entries that predate the
// eventlog; incremental changes are then applied from the log.
func (a *Application) preloadEntries(ctx context.Context) error {
	return a.preloadEntryPages(ctx, preloadPageSize, nil)
}

// preloadEntryPages is preloadEntries with the page size and a hook that runs
// after each page.
func (a *Application) preloadEntryPages(
	ctx context.Context, pageSize int64, afterPage func(),
) error {
	return preloadAll(ctx, pageSize,
		func(
			ctx context.Context, after *postgres.Entry, limit int64,
		) ([]postgres.Entry, error) {
			params := postgres.PreloadEntriesParams{Limit: limit}

			if after != nil {
				params.AfterLanguage = after.Language
				params.AfterEntry = after.Entry
			}

			rows, err := a.q.PreloadEntries(ctx, params)
			if err != nil {
				return nil, fmt.Errorf("list entries: %w", err)
			}

			return rows, nil
		},
		func(row postgres.Entry) {
			spell, ok := a.languages[row.Language]
			if !ok {
				return
			}

			if !a.loadableEntry(ctx, row) {
				return
			}

			spell.AddPhrase(entryAsPhrase(row))
		},
		afterPage,
	)
}

// preloadRules loads every current rule into its language's spellchecker, the
// startup baseline before the eventlog takes over.
func (a *Application) preloadRules(ctx context.Context) error {
	return a.preloadRulePages(ctx, preloadPageSize, nil)
}

// preloadRulePages is preloadRules with the page size and a hook that runs
// after each page.
func (a *Application) preloadRulePages(
	ctx context.Context, pageSize int64, afterPage func(),
) error {
	return preloadAll(ctx, pageSize,
		func(
			ctx context.Context, after *postgres.Rule, limit int64,
		) ([]postgres.Rule, error) {
			params := postgres.PreloadRulesParams{Limit: limit}

			if after != nil {
				params.AfterID = after.ID
			}

			rows, err := a.q.PreloadRules(ctx, params)
			if err != nil {
				return nil, fmt.Errorf("list rules: %w", err)
			}

			return rows, nil
		},
		func(row postgres.Rule) {
			spell, ok := a.languages[row.Language]
			if !ok {
				return
			}

			err := spell.AddRule(ruleDefFromRule(row))
			if err != nil {
				a.logger.ErrorContext(ctx, "skip invalid rule",
					"rule", row.Name, "language", row.Language,
					elephantine.LogKeyError, err)
			}
		},
		afterPage,
	)
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
	// A rule is keyed by its id alone, and its row names its language, so
	// the rule event finds its own spellchecker.
	if e.Kind == eventKindRule {
		return a.applyRuleEvent(ctx, e)
	}

	spell, ok := a.languages[e.Language]
	if !ok {
		return nil
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

	if !a.loadableEntry(ctx, entry) {
		s.RemovePhrase(e.Entry)

		return nil
	}

	s.AddPhrase(entryAsPhrase(entry))

	return nil
}

// loadableEntry reports whether an entry row can be loaded into a
// spellchecker. A row whose common mistakes expand to more strings than the
// expansion limit, or with a text, common mistake or form longer than the
// phrase window, predates the write validation (or a lowered limit) and is
// logged and skipped rather than failing the load or exhausting memory.
func (a *Application) loadableEntry(
	ctx context.Context, row postgres.Entry,
) bool {
	var forms map[string]string
	if row.Data != nil {
		forms = row.Data.Forms
	}

	err := CheckEntryLimits(
		row.Entry, row.CommonMistakes, forms, a.maxExpansions)
	if err == nil {
		return true
	}

	a.logger.WarnContext(ctx, "skip entry that cannot be matched",
		"language", row.Language, "entry", row.Entry,
		elephantine.LogKeyError, err)

	return false
}

// applyRuleEvent brings the spellcheckers in line with a rule event. Rule ids
// are unique across languages, and the row's own language decides which
// spellchecker holds the rule, not the language recorded on the event: the
// rule is removed from every other language, so an event that names the wrong
// language can neither activate the rule there nor leave its own language
// stale.
func (a *Application) applyRuleEvent(
	ctx context.Context, e postgres.Eventlog,
) error {
	// The eventlog carries the rule id as text for rule events.
	id, err := strconv.ParseInt(e.Entry, 10, 64)
	if err != nil {
		return fmt.Errorf("parse rule id %q: %w", e.Entry, err)
	}

	if e.Deleted {
		a.removeRuleExcept(id, "")

		return nil
	}

	row, err := a.q.GetRule(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		a.removeRuleExcept(id, "")

		return nil
	} else if err != nil {
		return fmt.Errorf("read rule from database: %w", err)
	}

	if row.Language != e.Language {
		a.logger.WarnContext(ctx, "rule event names another language than the rule",
			"rule", row.ID, "event_language", e.Language,
			"language", row.Language)
	}

	a.removeRuleExcept(id, row.Language)

	s, ok := a.languages[row.Language]
	if !ok {
		return nil
	}

	// A rule that fails to compile is logged and skipped rather than stalling
	// the eventlog; patterns are validated on write so this is defensive.
	err = s.AddRule(ruleDefFromRule(row))
	if err != nil {
		a.logger.ErrorContext(ctx, "skip invalid rule",
			"rule", row.ID, "language", row.Language,
			elephantine.LogKeyError, err)
	}

	return nil
}

// removeRuleExcept removes a rule from every language's spellchecker but the
// one named by keep; an empty keep removes it from all of them.
func (a *Application) removeRuleExcept(id int64, keep string) {
	for code, s := range a.languages {
		if code == keep {
			continue
		}

		s.RemoveRule(id)
	}
}

func ruleDefFromRule(r postgres.Rule) RuleDef {
	def := RuleDef{
		ID:          r.ID,
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
		def.CaseSensitive = r.Data.CaseSensitive
	}

	return def
}

func entryAsPhrase(e postgres.Entry) Phrase {
	var (
		forms         map[string]string
		caseSensitive bool
		g             guards
	)

	if e.Data != nil {
		forms = e.Data.Forms
		caseSensitive = e.Data.CaseSensitive
		g = compileGuards(
			e.Data.Before, e.Data.After,
			e.Data.NotBefore, e.Data.NotAfter, caseSensitive)
	}

	return Phrase{
		Text:           e.Entry,
		Description:    e.Description,
		CommonMistakes: e.CommonMistakes,
		Level:          e.Level,
		Forms:          forms,
		Status:         e.Status,
		CaseSensitive:  caseSensitive,
		Guards:         g,
	}
}
