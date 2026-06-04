package internal

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephant-api/spell"
	"github.com/ttab/elephant-spell/dictionaries"
	"github.com/ttab/elephant-spell/hunspell"
	"github.com/ttab/elephant-spell/postgres"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/pg"
	"github.com/ttab/howdah"
	"github.com/twitchtv/twirp"
	"golang.org/x/oauth2"
)

const (
	ScopeSpellcheckWrite = "spell_write"

	// EventlogChannel is the PostgreSQL NOTIFY channel used to signal that a
	// new eventlog entry is available. The payload is the new event id, but
	// it only serves as a wake-up: the consumer reads all events after its
	// own cursor.
	EventlogChannel = "eventlog"

	// eventPollInterval is the fallback period for draining the eventlog when
	// no notification arrives — the recovery backstop for a silently broken
	// LISTEN connection.
	eventPollInterval = time.Minute

	// eventlogRetention is how long events are kept before pruning. The window
	// only needs to comfortably exceed consumer lag (the fallback poll
	// interval plus listener reconnect/bounce recovery time); a restarting
	// replica reloads full state and resumes from the latest id, so older
	// events are never needed.
	eventlogRetention = time.Hour

	// eventlogPruneInterval is how often the prune job runs.
	eventlogPruneInterval = 15 * time.Minute
)

var (
	_ spell.Check        = &Application{}
	_ spell.Dictionaries = &Application{}
	_ spell.Rules        = &Application{}
)

type Parameters struct {
	Addr            string
	ProfileAddr     string
	TLSAddr         string
	CertFile        string
	KeyFile         string
	Logger          *slog.Logger
	Database        *pgxpool.Pool
	PubsubDatabase  *pgxpool.Pool
	AuthInfoParser  elephantine.AuthInfoParser
	Registerer      prometheus.Registerer
	CORSHosts       []string
	PingInterval    time.Duration
	PingGrace       time.Duration
	DefaultLanguage string

	// OIDC configuration for the web UI. When OIDCProvider is nil the
	// web UI is not served.
	OIDCProvider *oidc.Provider
	OIDCVerifier *oidc.IDTokenVerifier
	OIDCConfig   *oauth2.Config

	// Embedded filesystems for the web UI.
	Templates fs.FS
	Locales   fs.FS
	Assets    fs.FS
	Docs      fs.FS
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

	languages := make(map[string]*Spellcheck, len(supportedLanguages))

	// Instantiate one spellchecker per language.
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

		spell, err := NewSpellcheck(code, checker)
		if err != nil {
			return nil, fmt.Errorf("create spellchecker: %w", err)
		}

		languages[code] = spell
	}

	// FanOut signals the in-process consumer that the eventlog has advanced;
	// the Subscriber owns the single LISTEN connection (on the direct pool,
	// as LISTEN cannot go through PgBouncer) and provides built-in ping-based
	// health checking.
	updates := pg.NewFanOut[int64](EventlogChannel)

	subscriber := pg.NewSubscriber(
		p.Logger, p.PubsubDatabase,
		[]pg.ChannelSubscription{updates},
		pg.WithPingInterval(p.PingInterval),
		pg.WithPingGrace(p.PingGrace),
	)

	// Wire the fallback-poll recovery: when consecutive polls keep finding
	// work without an intervening notification the Subscriber is bounced to
	// rebuild a silently-broken LISTEN connection.
	err = updates.EnableRecovery(
		elephantine.NewMetricsHelper(p.Registerer),
		subscriber.Bounce,
	)
	if err != nil {
		return nil, fmt.Errorf("enable eventlog recovery: %w", err)
	}

	app := Application{
		p:          p,
		logger:     p.Logger,
		db:         p.Database,
		q:          postgres.New(p.Database),
		languages:  languages,
		updates:    updates,
		subscriber: subscriber,
	}

	return &app, nil
}

type Application struct {
	p          Parameters
	logger     *slog.Logger
	db         *pgxpool.Pool
	q          *postgres.Queries
	updates    *pg.FanOut[int64]
	subscriber *pg.Subscriber

	languages map[string]*Spellcheck
}

func (a *Application) Run(ctx context.Context) error {
	grace := elephantine.NewGracefulShutdown(a.logger, 10*time.Second)
	server := elephantine.NewAPIServer(
		a.logger, a.p.Addr, a.p.ProfileAddr,
		elephantine.APIServerCORSHosts(a.p.CORSHosts...),
		elephantine.APIServerTLS(a.p.TLSAddr, a.p.CertFile, a.p.KeyFile),
	)

	opts, err := elephantine.NewDefaultServiceOptions(
		a.logger, a.p.AuthInfoParser, a.p.Registerer,
		elephantine.ServiceAuthRequired,
	)
	if err != nil {
		return fmt.Errorf("set up service options: %w", err)
	}

	checkServer := spell.NewCheckServer(a, opts.ServerOptions())
	dictServer := spell.NewDictionariesServer(a, opts.ServerOptions())
	rulesServer := spell.NewRulesServer(a, opts.ServerOptions())

	server.RegisterAPI(checkServer, opts)
	server.RegisterAPI(dictServer, opts)
	server.RegisterAPI(rulesServer, opts)

	err = a.setupUI(server.Mux)
	if err != nil {
		return fmt.Errorf("set up web UI: %w", err)
	}

	grp := elephantine.NewErrGroup(ctx, a.logger)

	grp.Required("server", func(ctx context.Context) error {
		return server.ListenAndServe(grace.CancelOnQuit(ctx))
	})

	grp.Required("subscriber", func(ctx context.Context) error {
		return a.subscriber.Run(grace.CancelOnStop(ctx))
	})

	grp.Required("entry_updater", func(ctx context.Context) error {
		return a.runEntryUpdater(grace.CancelOnStop(ctx))
	})

	grp.Required("eventlog_pruner", func(ctx context.Context) error {
		return pg.RunInJobLock(
			grace.CancelOnStop(ctx), a.db, a.logger,
			"eventlog-pruner", "eventlog-prune",
			pg.JobLockOptions{},
			a.runEventlogPruner)
	})

	return grp.Wait()
}

// runEventlogPruner periodically deletes events past the retention window. It
// runs under a job lock so only one replica prunes at a time. A failed prune
// is logged but not fatal — the lock is kept and the next tick retries.
func (a *Application) runEventlogPruner(ctx context.Context) error {
	ticker := time.NewTicker(eventlogPruneInterval)
	defer ticker.Stop()

	for {
		before := pgtype.Timestamptz{
			Time:  time.Now().Add(-eventlogRetention),
			Valid: true,
		}

		removed, err := a.q.PruneEventlog(ctx, before)
		switch {
		case err != nil:
			a.logger.ErrorContext(ctx, "prune eventlog",
				elephantine.LogKeyError, err)
		case removed > 0:
			a.logger.InfoContext(ctx, "pruned eventlog",
				"removed", removed)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// runEntryUpdater keeps the spellcheckers in sync with the custom dictionary
// by following the eventlog. It preloads the current entries as a baseline,
// then drains the log on every notification ("wake up and drain") and on a
// periodic fallback tick. The fallback drain's result is reported to the
// FanOut so it can bounce the Subscriber when polling is the only thing
// keeping the consumer afloat.
func (a *Application) runEntryUpdater(ctx context.Context) error {
	// The notification payload is just a kick — the consumer always drains
	// every event past its cursor — so a buffer of one pending wake-up is
	// all the listener channel needs.
	events := make(chan int64, 1)

	go a.updates.ListenAll(ctx, events)

	// Read the cursor before preloading: preload then sees a database state
	// at least as new as the cursor, so every event up to the cursor is
	// already reflected and only later events need replaying. Events that
	// land between the two reads are simply replayed idempotently.
	cursor, err := a.q.GetLastEventID(ctx)
	if err != nil {
		return fmt.Errorf("read last event id: %w", err)
	}

	err = a.preloadEntries(ctx)
	if err != nil {
		return fmt.Errorf("preload entries: %w", err)
	}

	err = a.preloadRules(ctx)
	if err != nil {
		return fmt.Errorf("preload rules: %w", err)
	}

	// Catch up on anything written during startup before settling into the
	// notify/poll loop.
	_, cursor, err = a.drainEventlog(ctx, cursor)
	if err != nil {
		return fmt.Errorf("initial eventlog drain: %w", err)
	}

	ticker := time.NewTicker(eventPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-events:
			// Wire-side wake-up: the FanOut has already reset the
			// recovery streak, so we just drain. A transient failure is
			// not fatal — the next tick (or kick) retries from the same
			// cursor.
			_, cursor, err = a.drainEventlog(ctx, cursor)
			if err != nil {
				a.logger.ErrorContext(ctx, "drain eventlog on notification",
					elephantine.LogKeyError, err)
			}
		case <-ticker.C:
			var applied int

			applied, cursor, err = a.drainEventlog(ctx, cursor)
			if err != nil {
				a.logger.ErrorContext(ctx, "poll eventlog",
					elephantine.LogKeyError, err)

				continue
			}

			a.updates.Polled(applied)
		}
	}
}

func (a *Application) setupUI(mux *http.ServeMux) error {
	cAuth := howdah.NewOIDCAuth(
		a.p.OIDCProvider, a.p.OIDCVerifier, *a.p.OIDCConfig,
	)

	supportedLanguages := make([]string, 0, len(a.languages))
	for code := range a.languages {
		supportedLanguages = append(supportedLanguages, code)
	}

	cDicts := NewDictionariesUI(
		a.logger, cAuth, a.p.AuthInfoParser, a, a,
		supportedLanguages, a.p.DefaultLanguage,
	)

	cRules := NewRulesUI(
		a.logger, cAuth, a.p.AuthInfoParser, a, a,
		supportedLanguages, a.p.DefaultLanguage,
	)

	cLangs, err := NewLanguages(a.p.Locales)
	if err != nil {
		return fmt.Errorf("load languages: %w", err)
	}

	cUserInfo := NewUserInfo(a.logger, cAuth)

	cDocs, err := NewDocsUI(cAuth, a.p.Docs)
	if err != nil {
		return fmt.Errorf("create docs component: %w", err)
	}

	_, err = howdah.NewApplication(
		a.logger, mux,
		a.p.Templates, a.p.Locales, a.p.Assets,
		[]howdah.Component{cAuth, cLangs, cUserInfo, cDicts, cRules, cDocs},
	)
	if err != nil {
		return fmt.Errorf("create howdah application: %w", err)
	}

	return nil
}

// SupportedLanguages implements spell.Dictionaries.
func (a *Application) SupportedLanguages(
	ctx context.Context, req *spell.SupportedLanguagesRequest,
) (*spell.SupportedLanguagesResponse, error) {
	var res spell.SupportedLanguagesResponse

	for language := range a.languages {
		res.Languages = append(res.Languages, &spell.Language{
			Code: language,
		})
	}

	return &res, nil
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

	err = q.DeleteEntry(ctx, postgres.DeleteEntryParams{
		Language: req.Language,
		Entry:    req.Text,
	})
	if err != nil {
		return nil, twirp.InternalErrorf("write to database: %w", err)
	}

	err = a.recordChange(ctx, q, tx, req.Language, req.Text, true, eventKindEntry)
	if err != nil {
		return nil, twirp.InternalErrorf("record entry change: %w", err)
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

	level, err := entryLevelToRPC(row.Level)
	if err != nil {
		return nil, twirp.InternalErrorf("get entry level: %v", err)
	}

	var (
		forms         map[string]string
		caseSensitive bool
	)

	if row.Data != nil {
		forms = row.Data.Forms
		caseSensitive = row.Data.CaseSensitive
	}

	var updated string
	if row.Updated.Valid {
		updated = row.Updated.Time.Format(time.RFC3339)
	}

	res := spell.GetEntryResponse{
		Entry: &spell.CustomEntry{
			Language:       row.Language,
			Text:           row.Entry,
			Status:         row.Status,
			Description:    row.Description,
			CommonMistakes: row.CommonMistakes,
			Level:          level,
			Forms:          forms,
			Updated:        updated,
			UpdatedBy:      row.UpdatedBy,
			CaseSensitive:  caseSensitive,
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

	ruleRows, err := a.q.ListRuleCounts(ctx)
	if err != nil {
		return nil, twirp.InternalErrorf("read rule counts: %w", err)
	}

	// Merge per-language word and rule counts into one entry per language.
	byLang := make(map[string]*spell.CustomDictionary)

	dict := func(lang string) *spell.CustomDictionary {
		d, ok := byLang[lang]
		if !ok {
			d = &spell.CustomDictionary{Language: lang}
			byLang[lang] = d
		}

		return d
	}

	for _, row := range rows {
		d := dict(row.Language)
		d.EntryCount = row.Entries
		d.PendingCount = row.Pending
	}

	for _, row := range ruleRows {
		d := dict(row.Language)
		d.RuleCount = row.Rules
		d.RulePendingCount = row.Pending
	}

	var res spell.ListDictionariesResponse

	for _, d := range byLang {
		res.Dictionaries = append(res.Dictionaries, d)
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

	if strings.Contains(req.Query, "%") {
		return nil, twirp.InvalidArgumentError("query", "query cannot contain '%'")
	}

	var pattern string

	if req.Query != "" {
		pattern = "%" + req.Query + "%"
	}

	limit := req.PageSize
	if limit <= 0 {
		limit = 100
	}

	offset := limit * req.Page

	rows, err := a.q.ListEntries(ctx, postgres.ListEntriesParams{
		Language: pg.TextOrNull(req.Language),
		Query:    pg.TextOrNull(pattern),
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
		level, err := entryLevelToRPC(row.Level)
		if err != nil {
			return nil, twirp.InternalErrorf("get entry level: %v", err)
		}

		var (
			forms         map[string]string
			caseSensitive bool
		)

		if row.Data != nil {
			forms = row.Data.Forms
			caseSensitive = row.Data.CaseSensitive
		}

		var updated string
		if row.Updated.Valid {
			updated = row.Updated.Time.Format(time.RFC3339)
		}

		res.Entries[i] = &spell.CustomEntry{
			Language:       row.Language,
			Text:           row.Entry,
			Status:         row.Status,
			Description:    row.Description,
			CommonMistakes: row.CommonMistakes,
			Level:          level,
			Forms:          forms,
			Updated:        updated,
			UpdatedBy:      row.UpdatedBy,
			CaseSensitive:  caseSensitive,
		}
	}

	return &res, nil
}

// SetEntry implements spell.Dictionaries.
func (a *Application) SetEntry(
	ctx context.Context, req *spell.SetEntryRequest,
) (_ *spell.SetEntryResponse, outErr error) {
	auth, err := elephantine.RequireAnyScope(ctx, ScopeSpellcheckWrite)
	if err != nil {
		return nil, err //nolint: wrapcheck
	}

	if req.Entry == nil {
		return nil, twirp.RequiredArgumentError("entry")
	}

	if req.Entry.Language == "" {
		return nil, twirp.RequiredArgumentError("entry.language")
	}

	_, ok := a.languages[req.Entry.Language]
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

	level, err := entryLevelFromRPC(req.Entry.Level)
	if err != nil {
		return nil, err
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
		Level:          level,
		Data: &postgres.EntryData{
			Forms:         req.Entry.Forms,
			CaseSensitive: req.Entry.CaseSensitive,
		},
		Updated:   pgtype.Timestamptz{Time: time.Now(), Valid: true},
		UpdatedBy: auth.Claims.Subject,
	})
	if err != nil {
		return nil, twirp.InternalErrorf("write to database: %w", err)
	}

	err = a.recordChange(ctx, q, tx, req.Entry.Language, req.Entry.Text, false, eventKindEntry)
	if err != nil {
		return nil, twirp.InternalErrorf("record entry change: %w", err)
	}

	err = tx.Commit(ctx)
	if err != nil {
		return nil, twirp.InternalErrorf("commit changes: %w", err)
	}

	return &spell.SetEntryResponse{}, nil
}

// SetEntryStatus implements spell.Dictionaries. It updates only the moderation
// status of an existing entry — the lightweight path used by the accept/reject
// workflow.
func (a *Application) SetEntryStatus(
	ctx context.Context, req *spell.SetEntryStatusRequest,
) (_ *spell.SetEntryStatusResponse, outErr error) {
	auth, err := elephantine.RequireAnyScope(ctx, ScopeSpellcheckWrite)
	if err != nil {
		return nil, err //nolint: wrapcheck
	}

	if req.Language == "" {
		return nil, twirp.RequiredArgumentError("language")
	}

	if req.Text == "" {
		return nil, twirp.RequiredArgumentError("text")
	}

	if req.Status == "" {
		return nil, twirp.RequiredArgumentError("status")
	}

	tx, err := a.db.Begin(ctx)
	if err != nil {
		return nil, twirp.InternalErrorf("start transaction: %w", err)
	}

	defer pg.Rollback(tx, &outErr)

	q := a.q.WithTx(tx)

	affected, err := q.SetEntryStatus(ctx, postgres.SetEntryStatusParams{
		Language:  req.Language,
		Entry:     req.Text,
		Status:    req.Status,
		Updated:   pgtype.Timestamptz{Time: time.Now(), Valid: true},
		UpdatedBy: auth.Claims.Subject,
	})
	if err != nil {
		return nil, twirp.InternalErrorf("write to database: %w", err)
	}

	if affected == 0 {
		return nil, twirp.NotFoundError("entry does not exist")
	}

	err = a.recordChange(ctx, q, tx, req.Language, req.Text, false, eventKindEntry)
	if err != nil {
		return nil, twirp.InternalErrorf("record entry change: %w", err)
	}

	err = tx.Commit(ctx)
	if err != nil {
		return nil, twirp.InternalErrorf("commit changes: %w", err)
	}

	return &spell.SetEntryStatusResponse{}, nil
}

// ruleToRPC converts a stored rule row to its RPC representation.
func ruleToRPC(r postgres.Rule) (*spell.Rule, error) {
	level, err := entryLevelToRPC(r.Level)
	if err != nil {
		return nil, err
	}

	var updated string
	if r.Updated.Valid {
		updated = r.Updated.Time.Format(time.RFC3339)
	}

	out := &spell.Rule{
		Id:          r.ID,
		Language:    r.Language,
		Name:        r.Name,
		Status:      r.Status,
		Description: r.Description,
		Level:       level,
		Pattern:     r.Pattern,
		Replacement: r.Replacement,
		Updated:     updated,
		UpdatedBy:   r.UpdatedBy,
	}

	if r.Data != nil {
		out.Before = r.Data.Before
		out.After = r.Data.After
		out.NotBefore = r.Data.NotBefore
		out.NotAfter = r.Data.NotAfter
	}

	return out, nil
}

func entryLevelFromRPC(level spell.CorrectionLevel) (postgres.EntryLevel, error) {
	l := postgres.EntryLevelError

	switch level {
	case spell.CorrectionLevel_LEVEL_ERROR:
	case spell.CorrectionLevel_LEVEL_SUGGESTION:
		l = postgres.EntryLevelSuggestion
	case spell.CorrectionLevel_LEVEL_UNSPECIFIED:
	default:
		return "", twirp.InvalidArgumentError("level",
			"unhandled level")
	}

	return l, nil
}

func entryLevelToRPC(level postgres.EntryLevel) (spell.CorrectionLevel, error) {
	switch level {
	case postgres.EntryLevelError:
		return spell.CorrectionLevel_LEVEL_ERROR, nil
	case postgres.EntryLevelSuggestion:
		return spell.CorrectionLevel_LEVEL_SUGGESTION, nil
	default:
		return spell.CorrectionLevel_LEVEL_UNSPECIFIED,
			fmt.Errorf("unexpected postgres.EntryLevel: %#v", level)
	}
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

	lang, ok := a.languages[langCode]
	if !ok {
		return nil, twirp.InvalidArgument.Errorf(
			"unsupported language %q", req.Language)
	}

	res := spell.TextResponse{
		Misspelled: make([]*spell.Misspelled, len(req.Text)),
	}

	for i := range req.Text {
		m, err := lang.Check(ctx, req.Text[i], req.Suggestions, req.CustomOnly)
		if err != nil {
			return nil, fmt.Errorf(
				"spellcheck text %d: %v", i+1, err)
		}

		res.Misspelled[i] = m
	}

	return &res, nil
}

// Suggestions implements spell.Check.
func (a *Application) Suggestions(
	ctx context.Context,
	req *spell.SuggestionsRequest,
) (*spell.SuggestionsResponse, error) {
	_, ok := elephantine.GetAuthInfo(ctx)
	if !ok {
		return nil, twirp.Unauthenticated.Error("unauthenticated")
	}

	if req.Text == "" {
		return nil, twirp.RequiredArgumentError("text")
	}

	langCode := strings.ToLower(req.Language)

	lang, ok := a.languages[langCode]
	if !ok {
		return nil, twirp.InvalidArgument.Errorf(
			"unsupported language %q", req.Language)
	}

	sugg, err := lang.Suggestions(req.Text, req.CustomOnly)
	if err != nil {
		return nil, twirp.InternalErrorf(
			"generate suggestions: %v", err)
	}

	return &spell.SuggestionsResponse{
		Suggestions: sugg,
	}, nil
}

// eventKind values distinguish dictionary-word changes from rule changes in the
// eventlog so the consumer applies each to the right store.
const (
	eventKindEntry = "entry"
	eventKindRule  = "rule"
)

// recordChange appends a change to the eventlog and wakes the consumer. It must
// run inside the same transaction as the write so the change and its event
// commit atomically.
//
// The exclusive table lock serialises eventlog writers: a writer holds it
// until commit, so the next writer cannot draw its event id until this event
// is committed and visible. That keeps commit order equal to id order, which
// the log poller relies on to never skip an event. The published id is only a
// wake-up — the consumer reads all events after its own cursor.
func (a *Application) recordChange(
	ctx context.Context, q *postgres.Queries, db pg.DBExec,
	language, name string, deleted bool, kind string,
) error {
	err := q.LockEventlog(ctx)
	if err != nil {
		return fmt.Errorf("lock eventlog: %w", err)
	}

	id, err := q.InsertEvent(ctx, postgres.InsertEventParams{
		Language: language,
		Entry:    name,
		Deleted:  deleted,
		Kind:     kind,
	})
	if err != nil {
		return fmt.Errorf("append eventlog entry: %w", err)
	}

	err = a.updates.Publish(ctx, db, id)
	if err != nil {
		return fmt.Errorf("publish eventlog notification: %w", err)
	}

	return nil
}
