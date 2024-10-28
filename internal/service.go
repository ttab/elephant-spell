package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/blevesearch/segment"
	"github.com/dghubble/trie"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephant-api/spell"
	"github.com/ttab/elephant-spell/dictionaries"
	"github.com/ttab/elephant-spell/hunspell"
	"github.com/ttab/elephant-spell/postgres"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/pg"
	"github.com/twitchtv/twirp"
	"golang.org/x/sync/errgroup"
)

const (
	ScopeSpellcheckWrite = "spell_write"
)

type NotifyChannel string

const (
	NotifyEntryUpdate NotifyChannel = "entry_update"
)

var (
	_ spell.Check        = &Application{}
	_ spell.Dictionaries = &Application{}
)

type Parameters struct {
	Addr           string
	ProfileAddr    string
	Logger         *slog.Logger
	Database       *pgxpool.Pool
	AuthInfoParser *elephantine.AuthInfoParser
	Registerer     prometheus.Registerer
}

func NewApplication(
	ctx context.Context, p Parameters,
) (_ *Application, outErr error) {
	// We need to set up a directory with our dictionaries so that hunspell
	// can load them.
	tmpDir, err := os.MkdirTemp("", "spell-dicts-*")
	if err != nil {
		return nil, fmt.Errorf("create dictionary directory: %w", err)
	}

	defer func() {
		err := os.RemoveAll(tmpDir)
		if err != nil {
			outErr = errors.Join(outErr, fmt.Errorf(
				"clean up temporary dictionary files: %w", err))
		}
	}()

	dictFS := dictionaries.GetFS()

	dictFiles, err := dictFS.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("list embedded dictionaries: %w", err)
	}

	var supportedLanguages []string

	// Copy embedded dictionaries to the temp dir.
	for _, file := range dictFiles {
		name := filepath.Base(file.Name())

		data, err := fs.ReadFile(dictFS, file.Name())
		if err != nil {
			return nil, fmt.Errorf("read embedded dictionary %q: %w",
				name, err)
		}

		err = os.WriteFile(filepath.Join(tmpDir, name), data, 0o600)
		if err != nil {
			return nil, fmt.Errorf("copy embedded dictionary %q: %w",
				name, err)
		}

		language, ok := strings.CutSuffix(name, ".dic")
		if ok {
			supportedLanguages = append(supportedLanguages, language)
		}
	}

	checkers := make(map[string]*hunspell.Checker, len(supportedLanguages))
	phrases := make(map[string]*trie.RuneTrie)

	// Instantiate one hunspell checker per language.
	for _, lang := range supportedLanguages {
		checker, err := hunspell.NewChecker(
			filepath.Join(tmpDir, lang+".aff"),
			filepath.Join(tmpDir, lang+".dic"),
		)
		if err != nil {
			return nil, fmt.Errorf("create hunspell checker for %q: %w",
				lang, err)
		}

		// Convert from sv_SE to sv-se.
		code := strings.ToLower(strings.Replace(lang, "_", "-", 1))

		checkers[code] = checker
		phrases[code] = trie.NewRuneTrie()
	}

	app := Application{
		p:        p,
		logger:   p.Logger,
		db:       p.Database,
		q:        postgres.New(p.Database),
		checkers: checkers,
		phrases:  phrases,
	}

	return &app, nil
}

type Application struct {
	p            Parameters
	logger       *slog.Logger
	db           *pgxpool.Pool
	q            *postgres.Queries
	checkers     map[string]*hunspell.Checker
	entryUpdates chan EntryUpdateNotification

	m       sync.RWMutex
	phrases map[string]*trie.RuneTrie
}

func (a *Application) Run(ctx context.Context) error {
	grace := elephantine.NewGracefulShutdown(a.logger, 10*time.Second)
	server := elephantine.NewAPIServer(a.logger, a.p.Addr, a.p.ProfileAddr)

	opts, err := elephantine.NewDefaultServiceOptions(
		a.logger, a.p.AuthInfoParser, a.p.Registerer,
	)
	if err != nil {
		return fmt.Errorf("set up service options: %w", err)
	}

	checkServer := spell.NewCheckServer(a,
		twirp.WithServerJSONSkipDefaults(true),
		twirp.WithServerHooks(opts.Hooks))

	dictServer := spell.NewDictionariesServer(a,
		twirp.WithServerJSONSkipDefaults(true),
		twirp.WithServerHooks(opts.Hooks))

	server.RegisterAPI(checkServer, opts)
	server.RegisterAPI(dictServer, opts)

	grp := elephantine.NewErrGroup(ctx, a.logger)

	grp.Go("server", func(ctx context.Context) error {
		return server.ListenAndServe(grace.CancelOnQuit(ctx))
	})

	a.entryUpdates = make(chan EntryUpdateNotification, 16)

	grp.Go("notification_listener", func(ctx context.Context) error {
		defer close(a.entryUpdates)

		return a.runListener(grace.CancelOnStop(ctx))
	})

	grp.Go("entry_updater", func(ctx context.Context) error {
		err := a.preloadEntries(ctx)
		if err != nil {
			return fmt.Errorf("preload entries: %w", err)
		}

		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case n, ok := <-a.entryUpdates:
				if !ok {
					return nil
				}

				err := a.handleEntryUpdate(ctx, n)
				if err != nil {
					return fmt.Errorf("handle %s update of %q: %w",
						n.Language, n.Text, err)
				}
			}
		}
	})

	return grp.Wait()
}

// DeleteEntry implements spell.Dictionaries.
func (a *Application) DeleteEntry(
	ctx context.Context, req *spell.DeleteEntryRequest,
) (_ *spell.DeleteEntryResponse, outErr error) {
	_, err := elephantine.RequireAnyScope(ctx, ScopeSpellcheckWrite)
	if err != nil {
		return nil, err //nolint: wrapcheck
	}

	if req.Language == "" {
		return nil, twirp.RequiredArgumentError("language")
	}

	if req.Text == "" {
		return nil, twirp.RequiredArgumentError("text")
	}

	tx, err := a.db.Begin(ctx)
	if err != nil {
		return nil, twirp.InternalErrorf("start transaction: %w", err)
	}

	defer pg.Rollback(tx, &outErr)

	q := a.q.WithTx(tx)

	err = a.q.DeleteEntry(ctx, postgres.DeleteEntryParams{
		Language: req.Language,
		Entry:    req.Text,
	})
	if err != nil {
		return nil, twirp.InternalErrorf("write to database: %w", err)
	}

	err = notifyEntryUpdated(ctx, q, EntryUpdateNotification{
		Language: req.Language,
		Text:     req.Text,
		Deleted:  true,
	})
	if err != nil {
		return nil, twirp.InternalErrorf("send notification: %w", err)
	}

	err = tx.Commit(ctx)
	if err != nil {
		return nil, twirp.InternalErrorf("commit changes: %w", err)
	}

	return &spell.DeleteEntryResponse{}, nil
}

// GetEntry implements spell.Dictionaries.
func (a *Application) GetEntry(
	ctx context.Context, req *spell.GetEntryRequest,
) (*spell.GetEntryResponse, error) {
	_, err := elephantine.RequireAnyScope(ctx, ScopeSpellcheckWrite)
	if err != nil {
		return nil, err //nolint: wrapcheck
	}

	if req.Language == "" {
		return nil, twirp.RequiredArgumentError("language")
	}

	if req.Text == "" {
		return nil, twirp.RequiredArgumentError("text")
	}

	row, err := a.q.GetEntry(ctx, postgres.GetEntryParams{
		Language: req.Language,
		Entry:    req.Text,
	})
	if err != nil {
		return nil, twirp.InternalErrorf("read from database: %w", err)
	}

	res := spell.GetEntryResponse{
		Entry: &spell.CustomEntry{
			Language:       row.Language,
			Text:           row.Entry,
			Status:         row.Status,
			Description:    row.Description,
			CommonMistakes: row.CommonMistakes,
		},
	}

	return &res, nil
}

// ListDictionaries implements spell.Dictionaries.
func (a *Application) ListDictionaries(
	ctx context.Context, req *spell.ListDictionariesRequest,
) (*spell.ListDictionariesResponse, error) {
	_, err := elephantine.RequireAnyScope(ctx, ScopeSpellcheckWrite)
	if err != nil {
		return nil, err //nolint: wrapcheck
	}

	rows, err := a.q.ListDictionaries(ctx)
	if err != nil {
		return nil, twirp.InternalErrorf("read from database: %w", err)
	}

	res := spell.ListDictionariesResponse{
		Dictionaries: make([]*spell.CustomDictionary, len(rows)),
	}

	for i, row := range rows {
		res.Dictionaries[i] = &spell.CustomDictionary{
			Language:   row.Language,
			EntryCount: row.Entries,
		}
	}

	return &res, nil
}

// ListEntries implements spell.Dictionaries.
func (a *Application) ListEntries(
	ctx context.Context,
	req *spell.ListEntriesRequest,
) (*spell.ListEntriesResponse, error) {
	_, err := elephantine.RequireAnyScope(ctx, ScopeSpellcheckWrite)
	if err != nil {
		return nil, err //nolint: wrapcheck
	}

	if strings.Contains(req.Prefix, "%") {
		return nil, twirp.InvalidArgumentError("prefix", "prefix cannot contain '%'")
	}

	var pattern string

	if req.Prefix != "" {
		pattern = req.Prefix + "%"
	}

	limit := int64(100)
	offset := limit * req.Page

	rows, err := a.q.ListEntries(ctx, postgres.ListEntriesParams{
		Language: pg.TextOrNull(req.Language),
		Pattern:  pg.TextOrNull(pattern),
		Status:   pg.TextOrNull(req.Status),
		Limit:    limit,
		Offset:   offset,
	})
	if err != nil {
		return nil, twirp.InternalErrorf("read from database: %w", err)
	}

	res := spell.ListEntriesResponse{
		Entries: make([]*spell.CustomEntry, len(rows)),
	}

	for i, row := range rows {
		res.Entries[i] = &spell.CustomEntry{
			Language:       row.Language,
			Text:           row.Entry,
			Status:         row.Status,
			Description:    row.Description,
			CommonMistakes: row.CommonMistakes,
		}
	}

	return &res, nil
}

// SetEntry implements spell.Dictionaries.
func (a *Application) SetEntry(
	ctx context.Context, req *spell.SetEntryRequest,
) (_ *spell.SetEntryResponse, outErr error) {
	_, err := elephantine.RequireAnyScope(ctx, ScopeSpellcheckWrite)
	if err != nil {
		return nil, err //nolint: wrapcheck
	}

	if req.Entry == nil {
		return nil, twirp.RequiredArgumentError("entry")
	}

	if req.Entry.Language == "" {
		return nil, twirp.RequiredArgumentError("entry.language")
	}

	_, ok := a.checkers[req.Entry.Language]
	if !ok {
		return nil, twirp.InvalidArgumentError("entry.language",
			fmt.Sprintf("unknown language %q", req.Entry.Language))
	}

	if req.Entry.Text == "" {
		return nil, twirp.RequiredArgumentError("entry.text")
	}

	if req.Entry.Status == "" {
		return nil, twirp.RequiredArgumentError("entry.status")
	}

	tx, err := a.db.Begin(ctx)
	if err != nil {
		return nil, twirp.InternalErrorf("start transaction: %w", err)
	}

	defer pg.Rollback(tx, &outErr)

	q := a.q.WithTx(tx)

	err = q.SetEntry(ctx, postgres.SetEntryParams{
		Language:       req.Entry.Language,
		Entry:          req.Entry.Text,
		Status:         req.Entry.Status,
		Description:    req.Entry.Description,
		CommonMistakes: req.Entry.CommonMistakes,
	})
	if err != nil {
		return nil, twirp.InternalErrorf("write to database: %w", err)
	}

	err = notifyEntryUpdated(ctx, q, EntryUpdateNotification{
		Language: req.Entry.Language,
		Text:     req.Entry.Text,
	})
	if err != nil {
		return nil, twirp.InternalErrorf("send notification: %w", err)
	}

	err = tx.Commit(ctx)
	if err != nil {
		return nil, twirp.InternalErrorf("commit changes: %w", err)
	}

	return &spell.SetEntryResponse{}, nil
}

// Text implements spell.Check.
func (a *Application) Text(
	ctx context.Context, req *spell.TextRequest,
) (*spell.TextResponse, error) {
	_, ok := elephantine.GetAuthInfo(ctx)
	if !ok {
		return nil, twirp.Unauthenticated.Error("unauthenticated")
	}

	langCode := strings.ToLower(req.Language)

	checker, ok := a.checkers[langCode]
	if !ok {
		return nil, twirp.InvalidArgument.Errorf("unsupported language %q", req.Language)
	}

	res := spell.TextResponse{
		Misspelled: make([]*spell.Misspelled, len(req.Text)),
	}

	for i := range req.Text {
		res.Misspelled[i] = a.spellcheck(req.Text[i], checker, langCode)
	}

	return &res, nil
}

func (a *Application) spellcheck(
	text string, checker *hunspell.Checker, langCode string,
) *spell.Misspelled {
	var res spell.Misspelled

	textData := []byte(text)

	a.m.RLock()
	trie := a.phrases[langCode]

	for text := range PhraseIterator(textData, 3) {
		v := trie.Get(text)

		p, ok := v.(*phrase)
		if !ok {
			continue
		}

		if p.Text != text {
			// Make sure that we only act once on a custom entry.
			oldNews := slices.ContainsFunc(res.Entries,
				func(m *spell.MisspelledEntry) bool {
					return m.Text == text
				})
			if oldNews {
				continue
			}

			res.Entries = append(res.Entries,
				&spell.MisspelledEntry{
					Text: text,
					Suggestions: []*spell.Suggestion{
						{
							Text:        p.Text,
							Description: p.Description,
						},
					},
				})
		}

		textData = bytes.ReplaceAll(textData, []byte(text), nil)
	}

	a.m.RUnlock()

	seg := segment.NewSegmenter(bytes.NewReader(textData))

	seen := make(map[string]bool)

	for seg.Segment() {
		if seg.Type() != segment.Letter {
			continue
		}

		word := seg.Text()

		if seen[word] {
			continue
		}

		seen[word] = true

		correct := checker.Spell(word)
		if correct {
			continue
		}

		var suggestions []*spell.Suggestion

		for _, sugg := range checker.Suggest(word) {
			suggestions = append(suggestions, &spell.Suggestion{
				Text: sugg,
			})
		}

		res.Entries = append(res.Entries, &spell.MisspelledEntry{
			Text:        word,
			Suggestions: suggestions,
		})
	}

	return &res
}

type EntryUpdateNotification struct {
	Language string
	Text     string
	Deleted  bool
}

func (a *Application) runListener(ctx context.Context) (outErr error) {
	conn, err := a.db.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("failed to acquire connection from pool: %w", err)
	}

	pConn := conn.Hijack()

	defer func() {
		err := pConn.Close(ctx)
		if err != nil {
			outErr = errors.Join(outErr, fmt.Errorf(
				"failed to close PG listen connection: %w", err))
		}
	}()

	notifications := []NotifyChannel{
		NotifyEntryUpdate,
	}

	for _, channel := range notifications {
		ident := pgx.Identifier{string(channel)}

		_, err := pConn.Exec(ctx, "LISTEN "+ident.Sanitize())
		if err != nil {
			return fmt.Errorf("failed to start listening to %q: %w",
				channel, err)
		}
	}

	received := make(chan *pgconn.Notification)
	grp, gCtx := errgroup.WithContext(ctx)

	grp.Go(func() error {
		for {
			notification, err := pConn.WaitForNotification(gCtx)
			if err != nil {
				return fmt.Errorf(
					"error while waiting for notification: %w", err)
			}

			received <- notification
		}
	})

	grp.Go(func() error {
		for {
			var notification *pgconn.Notification

			select {
			case <-ctx.Done():
				return ctx.Err()
			case notification = <-received:
			}

			switch NotifyChannel(notification.Channel) {
			case NotifyEntryUpdate:
				var n EntryUpdateNotification

				err := json.Unmarshal(
					[]byte(notification.Payload), &n)
				if err != nil {
					break
				}

				a.entryUpdates <- n
			}
		}
	})

	err = grp.Wait()
	if err != nil {
		return err //nolint:wrapcheck
	}

	return nil
}

func notifyEntryUpdated(
	ctx context.Context, q *postgres.Queries,
	payload EntryUpdateNotification,
) error {
	return pgNotify(ctx, q, NotifyEntryUpdate, payload)
}

func pgNotify[T any](
	ctx context.Context, q *postgres.Queries,
	channel NotifyChannel, payload T,
) error {
	message, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload for notification: %w", err)
	}

	err = q.Notify(ctx, postgres.NotifyParams{
		Channel: string(channel),
		Message: string(message),
	})
	if err != nil {
		return fmt.Errorf("failed to publish notification payload to channel: %w", err)
	}

	return nil
}
