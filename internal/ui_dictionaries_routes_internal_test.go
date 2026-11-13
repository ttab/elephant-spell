package internal

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ttab/elephant-api/spell"
	"github.com/ttab/howdah"
)

// routeRecorder stands in for howdah.PageMux: it registers the page handlers
// on a plain http.ServeMux and records which route served a request, with
// what entry text, and what the handler returned.
type routeRecorder struct {
	mux *http.ServeMux

	pattern string
	text    string
	page    *howdah.Page
	err     error
}

func newRouteRecorder() *routeRecorder {
	return &routeRecorder{mux: http.NewServeMux()}
}

func (rr *routeRecorder) HandleFunc(pattern string, handler howdah.PageHandlerFunc) {
	rr.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		rr.pattern = pattern
		rr.text = r.PathValue("text")
		rr.page, rr.err = handler(r.Context(), w, r)
	})
}

func (rr *routeRecorder) serve(r *http.Request) *httptest.ResponseRecorder {
	rr.pattern, rr.text, rr.page, rr.err = "", "", nil, nil

	w := httptest.NewRecorder()

	rr.mux.ServeHTTP(w, r)

	return w
}

type passAuth struct{}

func (passAuth) RequireAuth(
	ctx context.Context, _ http.ResponseWriter, _ *http.Request,
) (context.Context, error) {
	return ctx, nil
}

// entryDictionaries serves every requested entry as an existing one.
type entryDictionaries struct {
	spell.Dictionaries

	requested []string
}

func (d *entryDictionaries) GetEntry(
	_ context.Context, req *spell.GetEntryRequest,
) (*spell.GetEntryResponse, error) {
	d.requested = append(d.requested, req.Text)

	return &spell.GetEntryResponse{
		Entry: &spell.CustomEntry{
			Language: req.Language,
			Text:     req.Text,
			Status:   "accepted",
		},
	}, nil
}

// TestDictionaryEntryRoutes checks that an entry can be opened and saved
// whatever its text, including texts that are also the names of the
// language's own routes.
func TestDictionaryEntryRoutes(t *testing.T) {
	const lang = "sv-se"

	dicts := &entryDictionaries{}

	ui := NewDictionariesUI(slog.New(slog.DiscardHandler),
		passAuth{}, nil, dicts, nil, []string{lang}, lang)

	rr := newRouteRecorder()

	ui.registerRoutes(rr)

	texts := []string{
		"new", "entries", "_new", "_validate", "_expansions",
		"rename", "delete", "Mexico City", "a/b",
	}

	for _, text := range texts {
		t.Run(text, func(t *testing.T) {
			path := entryPath(lang, text)

			open := httptest.NewRequest(http.MethodGet, path, nil)
			open.Header.Set("HX-Request", "true")

			rr.serve(open)

			if rr.pattern != "GET /dictionaries/{language}/entries/{text}" {
				t.Fatalf("GET %s served by %q", path, rr.pattern)
			}

			if rr.err != nil {
				t.Fatalf("open entry: %v", rr.err)
			}

			contents, ok := rr.page.Contents.(dictionariesContents)
			if !ok || contents.Entry == nil || contents.Entry.Entry != text {
				t.Fatalf("GET %s opened %+v, want entry %q",
					path, rr.page.Contents, text)
			}

			if dicts.requested[len(dicts.requested)-1] != text {
				t.Fatalf("GET %s fetched entry %q, want %q",
					path, dicts.requested[len(dicts.requested)-1], text)
			}

			for _, op := range []struct {
				method, suffix, pattern string
			}{
				{http.MethodPost, "", "POST /dictionaries/{language}/entries/{text}"},
				{http.MethodPost, "/delete", "POST /dictionaries/{language}/entries/{text}/delete"},
				{http.MethodGet, "/rename", "GET /dictionaries/{language}/entries/{text}/rename"},
				{http.MethodPost, "/rename", "POST /dictionaries/{language}/entries/{text}/rename"},
			} {
				req := httptest.NewRequest(op.method, path+op.suffix,
					strings.NewReader(url.Values{"level": {"error"}}.Encode()))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

				rr.serve(req)

				if rr.pattern != op.pattern || rr.text != text {
					t.Errorf("%s %s%s served by %q for %q, want %q for %q",
						op.method, path, op.suffix, rr.pattern, rr.text,
						op.pattern, text)
				}
			}
		})
	}

	t.Run("fixed routes", func(t *testing.T) {
		for _, c := range []struct {
			method, path, pattern string
		}{
			{http.MethodGet, "/dictionaries/sv-se/new", "GET /dictionaries/{language}/new"},
			{http.MethodGet, "/dictionaries/sv-se/entries", "GET /dictionaries/{language}/entries"},
			{http.MethodPost, "/dictionaries/sv-se/_new", "POST /dictionaries/{language}/_new"},
			{http.MethodPost, "/dictionaries/sv-se/_validate", "POST /dictionaries/{language}/_validate"},
			{http.MethodPost, "/dictionaries/sv-se/_expansions", "POST /dictionaries/{language}/_expansions"},
		} {
			_, pattern := rr.mux.Handler(httptest.NewRequest(c.method, c.path, nil))
			if pattern != c.pattern {
				t.Errorf("%s %s served by %q, want %q",
					c.method, c.path, pattern, c.pattern)
			}
		}
	})

	t.Run("old entry links redirect", func(t *testing.T) {
		w := rr.serve(httptest.NewRequest(http.MethodGet,
			"/dictionaries/sv-se/Mexico%20City", nil))

		if w.Code != http.StatusMovedPermanently {
			t.Fatalf("got status %d, want %d", w.Code, http.StatusMovedPermanently)
		}

		want := "/dictionaries/sv-se/entries/Mexico%20City"
		if got := w.Header().Get("Location"); got != want {
			t.Fatalf("redirected to %q, want %q", got, want)
		}
	})
}
