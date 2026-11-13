package internal

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	connect "connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephant-api/spell"
	"github.com/ttab/elephant-spell/postgres"
	"github.com/ttab/elephant-spell/schema"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/pg"
	"github.com/ttab/eltest"
	"github.com/ttab/howdah"
)

// robustnessDB starts a migrated Postgres for one test.
func robustnessDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	pgEnv := eltest.NewPostgres(t, eltest.Postgres17_6)
	env := pgEnv.Database(t, "spell", schema.Migrations, true)

	pool, err := pgxpool.New(t.Context(), env.PostgresURI)
	eltest.Must(t, err, "create connection pool")
	t.Cleanup(pool.Close)

	return pool
}

func storeEntry(t *testing.T, q *postgres.Queries, entry string, mistakes ...string) {
	t.Helper()

	err := q.SetEntry(t.Context(), postgres.SetEntryParams{
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

func storeRule(t *testing.T, q *postgres.Queries, language, name string) int64 {
	t.Helper()

	id, err := q.InsertRule(t.Context(), postgres.InsertRuleParams{
		Language:    language,
		Name:        name,
		Status:      "accepted",
		Level:       postgres.EntryLevelError,
		Pattern:     name + " kr",
		Replacement: name + " kronor",
		Updated:     pgtype.Timestamptz{Time: time.Now(), Valid: true},
		UpdatedBy:   "test",
	})
	eltest.Must(t, err, "write rule %q", name)

	return id
}

// TestRuleEventUsesTheRuleLanguage checks that a rule event that names
// another language than the rule's own loads the rule into its own
// language's spellchecker, and takes it out of the language the event named.
func TestRuleEventUsesTheRuleLanguage(t *testing.T) {
	pool := robustnessDB(t)
	q := postgres.New(pool)

	id := storeRule(t, q, "sv-se", "femton")

	svSE := newCasingCheck(t)
	enGB := newCasingCheck(t)

	app := &Application{
		logger: slog.New(slog.DiscardHandler),
		q:      q,
		languages: map[string]*Spellcheck{
			"sv-se": svSE,
			"en-gb": enGB,
		},
	}

	// As if an earlier mismatched event had already put it there.
	err := enGB.AddRule(RuleDef{ID: id, Pattern: "femton kr"})
	eltest.Must(t, err, "add the rule to en-gb")

	err = app.applyEvent(t.Context(), postgres.Eventlog{
		Language: "en-gb",
		Entry:    strconv.FormatInt(id, 10),
		Kind:     eventKindRule,
	})
	eltest.Must(t, err, "apply the rule event")

	if _, ok := svSE.rules[id]; !ok {
		t.Error("the rule is not active for its own language, sv-se")
	}

	if _, ok := enGB.rules[id]; ok {
		t.Error("the rule is active for en-gb, the language the event named")
	}
}

// TestPreloadPagesByKey checks that a row deleted from a page the preload
// has already read does not make it skip a row on a later page.
func TestPreloadPagesByKey(t *testing.T) {
	pool := robustnessDB(t)
	q := postgres.New(pool)

	texts := []string{"Aaland", "Bbland", "Ccland", "Ddland", "Eeland"}

	for _, text := range texts {
		storeEntry(t, q, text)
	}

	var ruleIDs []int64

	for _, name := range []string{"ett", "två", "tre", "fyra", "fem"} {
		ruleIDs = append(ruleIDs, storeRule(t, q, "sv-se", name))
	}

	check := newCasingCheck(t)

	app := &Application{
		logger:    slog.New(slog.DiscardHandler),
		q:         q,
		languages: map[string]*Spellcheck{"sv-se": check},

		maxExpansions: DefaultMaxExpansions,
	}

	ctx := t.Context()

	// Delete the first row once the first page of two has been read, as a
	// concurrent writer would.
	var pages int

	err := app.preloadEntryPages(ctx, 2, func() {
		pages++

		if pages != 1 {
			return
		}

		err := q.DeleteEntry(ctx, postgres.DeleteEntryParams{
			Language: "sv-se", Entry: texts[0],
		})
		eltest.Must(t, err, "delete the first entry")
	})
	eltest.Must(t, err, "preload entries")

	for _, text := range texts {
		if _, ok := check.entries[text]; !ok {
			t.Errorf("entry %q was not preloaded", text)
		}
	}

	pages = 0

	err = app.preloadRulePages(ctx, 2, func() {
		pages++

		if pages != 1 {
			return
		}

		_, err := q.DeleteRule(ctx, ruleIDs[0])
		eltest.Must(t, err, "delete the first rule")
	})
	eltest.Must(t, err, "preload rules")

	for _, id := range ruleIDs {
		if _, ok := check.rules[id]; !ok {
			t.Errorf("rule %d was not preloaded", id)
		}
	}
}

// TestDictionaryReadiness checks that the readiness check fails until the
// entry updater has preloaded the custom dictionary, and that once it passes
// the stored entries are loaded.
func TestDictionaryReadiness(t *testing.T) {
	pool := robustnessDB(t)
	q := postgres.New(pool)

	storeEntry(t, q, "till exempel", "tex")

	check := newCasingCheck(t)
	logger := slog.New(slog.DiscardHandler)

	app := &Application{
		logger:    logger,
		db:        pool,
		q:         q,
		languages: map[string]*Spellcheck{"sv-se": check},
		updates:   pg.NewFanOut[int64](EventlogChannel),

		maxExpansions: DefaultMaxExpansions,
	}

	health := elephantine.NewTestHealthServer(logger,
		elephantine.WithHealthServerRegisterer(prometheus.NewRegistry()))

	t.Cleanup(func() {
		err := health.Close()
		if err != nil {
			t.Errorf("close the health server: %v", err)
		}
	})

	app.registerReadyChecks(health)

	readyURL := "http://" + health.Addr() + "/health/ready"

	ready := func() bool {
		req, err := http.NewRequestWithContext(
			t.Context(), http.MethodGet, readyURL, nil)
		eltest.Must(t, err, "create the readiness request")

		res, err := http.DefaultClient.Do(req)
		eltest.Must(t, err, "request readiness")

		_, err = io.Copy(io.Discard, res.Body)
		eltest.Must(t, err, "read the readiness response")

		err = res.Body.Close()
		eltest.Must(t, err, "close the readiness response")

		return res.StatusCode == http.StatusOK
	}

	if ready() {
		t.Fatal("ready before the custom dictionary was loaded")
	}

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)

	go func() {
		done <- app.runEntryUpdater(ctx)
	}()

	t.Cleanup(func() {
		cancel()

		err := <-done
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("entry updater: %v", err)
		}
	})

	deadline := time.Now().Add(30 * time.Second)

	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("not ready after the entry updater started")
		}

		time.Sleep(20 * time.Millisecond)
	}

	got := misspelledTexts(t, check, "Det var tex så.", true)
	wantSuggestions(t, got, "tex", "till exempel")
}

// TestPageWindow checks the paging of the list RPCs: negative pages are
// refused, the page size defaults and is capped, and an offset cannot
// overflow.
func TestPageWindow(t *testing.T) {
	cases := []struct {
		page, size    int64
		limit, offset int64
		refused       bool
	}{
		{page: 0, size: 0, limit: DefaultPageSize, offset: 0},
		{page: 2, size: 10, limit: 10, offset: 20},
		{page: 1, size: MaxPageSize, limit: MaxPageSize, offset: MaxPageSize},
		{page: 1, size: 100000, limit: MaxPageSize, offset: MaxPageSize},
		{page: 0, size: -5, limit: DefaultPageSize, offset: 0},
		{page: -1, size: 10, refused: true},
		{page: math.MaxInt64 / 10, size: 100, refused: true},
	}

	for _, tc := range cases {
		limit, offset, err := pageWindow(tc.page, tc.size)

		if tc.refused {
			var cErr *connect.Error
			if !errors.As(err, &cErr) ||
				cErr.Code() != connect.CodeInvalidArgument {
				t.Errorf("page %d size %d: got %v, want invalid argument",
					tc.page, tc.size, err)
			}

			continue
		}

		if err != nil {
			t.Errorf("page %d size %d: %v", tc.page, tc.size, err)

			continue
		}

		if limit != tc.limit || offset != tc.offset {
			t.Errorf("page %d size %d: got limit %d offset %d, want %d %d",
				tc.page, tc.size, limit, offset, tc.limit, tc.offset)
		}
	}

	if MaxPageSize < moderationFetch {
		t.Errorf("MaxPageSize %d is below the moderation fetch of %d",
			MaxPageSize, moderationFetch)
	}
}

// pagingServices records the list requests the UI makes and answers them
// with a fixed set of pending entries and a rule in another language.
type pagingServices struct {
	spell.Dictionaries
	spell.Rules

	entries []*spell.CustomEntry

	listEntries []*spell.ListEntriesRequest
	setRules    int
	deleteRules int
}

func (s *pagingServices) ListDictionaries(
	context.Context, *spell.ListDictionariesRequest,
) (*spell.ListDictionariesResponse, error) {
	return &spell.ListDictionariesResponse{}, nil
}

func (s *pagingServices) ListEntries(
	_ context.Context, req *spell.ListEntriesRequest,
) (*spell.ListEntriesResponse, error) {
	s.listEntries = append(s.listEntries, req)

	return &spell.ListEntriesResponse{Entries: s.entries}, nil
}

func (s *pagingServices) ListRules(
	context.Context, *spell.ListRulesRequest,
) (*spell.ListRulesResponse, error) {
	return &spell.ListRulesResponse{}, nil
}

func (s *pagingServices) GetRule(
	_ context.Context, req *spell.GetRuleRequest,
) (*spell.GetRuleResponse, error) {
	return &spell.GetRuleResponse{Rule: &spell.Rule{
		Id: req.Id, Language: "sv-se", Name: "femton",
		Status: "accepted", Pattern: "femton kr",
	}}, nil
}

func (s *pagingServices) SetRule(
	_ context.Context, req *spell.SetRuleRequest,
) (*spell.SetRuleResponse, error) {
	s.setRules++

	return &spell.SetRuleResponse{Id: req.Rule.Id}, nil
}

func (s *pagingServices) DeleteRule(
	context.Context, *spell.DeleteRuleRequest,
) (*spell.DeleteRuleResponse, error) {
	s.deleteRules++

	return &spell.DeleteRuleResponse{}, nil
}

func httpStatus(t *testing.T, err error) int {
	t.Helper()

	var hErr *howdah.HTTPError
	if !errors.As(err, &hErr) {
		t.Fatalf("got %v, want an HTTP error", err)
	}

	return hErr.Code
}

// TestModerationPaging checks that the moderation page refuses a negative
// page and renders an empty page, rather than panicking, for one far past
// the end of the queue.
func TestModerationPaging(t *testing.T) {
	const lang = "sv-se"

	svc := &pagingServices{}

	for i := range 25 {
		svc.entries = append(svc.entries, &spell.CustomEntry{
			Language: lang, Text: "ord" + strconv.Itoa(i), Status: statusPending,
		})
	}

	ui := NewDictionariesUI(slog.New(slog.DiscardHandler),
		passAuth{}, nil, svc, svc, []string{lang}, lang)

	for _, page := range []string{"-1", "-9223372036854775808"} {
		r := httptest.NewRequest(http.MethodGet,
			"/moderation/"+lang+"/?page="+page, nil)
		r.SetPathValue("language", lang)

		_, err := ui.moderationPage(t.Context(), httptest.NewRecorder(), r)
		if status := httpStatus(t, err); status != http.StatusBadRequest {
			t.Errorf("page %s: got status %d, want %d",
				page, status, http.StatusBadRequest)
		}
	}

	for _, page := range []int64{3, math.MaxInt64 / 5, math.MaxInt64} {
		contents, err := ui.moderationData(t.Context(), lang, page)
		eltest.Must(t, err, "moderation data for page %d", page)

		if len(contents.Items) != 0 || contents.HasMore {
			t.Errorf("page %d: got %d items, more %v, want an empty last page",
				page, len(contents.Items), contents.HasMore)
		}
	}

	contents, err := ui.moderationData(t.Context(), lang, 2)
	eltest.Must(t, err, "moderation data for the last page")

	if len(contents.Items) != 5 {
		t.Errorf("last page: got %d items, want 5", len(contents.Items))
	}
}

// TestEntryListPageSize checks that the dictionary UI asks for the page size
// it uses to tell whether there is another page.
func TestEntryListPageSize(t *testing.T) {
	svc := &pagingServices{}

	ui := NewDictionariesUI(slog.New(slog.DiscardHandler),
		passAuth{}, nil, svc, svc, []string{"sv-se"}, "sv-se")

	_, _, err := ui.listEntries(t.Context(), "sv-se", "", 0)
	eltest.Must(t, err, "list entries")

	if len(svc.listEntries) != 1 ||
		svc.listEntries[0].PageSize != entriesPageSize {
		t.Fatalf("list requests %+v, want one with page size %d",
			svc.listEntries, entriesPageSize)
	}
}

// TestRulePagesCheckTheLanguage checks that a rule's pages answer not found
// when the rule belongs to another language than the path names, and that
// such a request neither saves nor deletes the rule.
func TestRulePagesCheckTheLanguage(t *testing.T) {
	svc := &pagingServices{}

	ui := NewRulesUI(slog.New(slog.DiscardHandler),
		passAuth{}, nil, svc, svc, []string{"en-gb", "sv-se"}, "sv-se")

	ctx := t.Context()

	request := func(method, path string) *http.Request {
		r := httptest.NewRequestWithContext(ctx, method, path, nil)
		r.SetPathValue("language", "en-gb")
		r.SetPathValue("id", "7")

		if method == http.MethodPost {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}

		return r
	}

	handlers := []struct {
		name   string
		method string
		path   string
		fn     howdah.PageHandlerFunc
	}{
		{"open", http.MethodGet, "/rules/en-gb/7", ui.rulePage},
		{"save", http.MethodPost, "/rules/en-gb/7", ui.saveRule},
		{"delete", http.MethodPost, "/rules/en-gb/7/delete", ui.deleteRule},
	}

	for _, h := range handlers {
		_, err := h.fn(ctx, httptest.NewRecorder(), request(h.method, h.path))
		if status := httpStatus(t, err); status != http.StatusNotFound {
			t.Errorf("%s: got status %d, want %d",
				h.name, status, http.StatusNotFound)
		}
	}

	if svc.setRules != 0 || svc.deleteRules != 0 {
		t.Errorf("rule written through the wrong language: %d saves, %d deletes",
			svc.setRules, svc.deleteRules)
	}
}
