package internal

import (
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/ttab/elephant-api/spell"
	"github.com/ttab/elephantine"
	"github.com/ttab/howdah"
	"github.com/twitchtv/twirp"
)

type DictionariesUI struct {
	log        *slog.Logger
	auth       howdah.Authenticator
	authParser elephantine.AuthInfoParser
	dicts      spell.Dictionaries
	languages  []string
}

func NewDictionariesUI(
	logger *slog.Logger,
	auth howdah.Authenticator,
	authParser elephantine.AuthInfoParser,
	dicts spell.Dictionaries,
	languages []string,
) *DictionariesUI {
	slices.Sort(languages)

	return &DictionariesUI{
		log:        logger,
		auth:       auth,
		authParser: authParser,
		dicts:      dicts,
		languages:  languages,
	}
}

func (d *DictionariesUI) GetTemplateFuncs() template.FuncMap {
	return template.FuncMap{
		"pathEscape": url.PathEscape,
		"add":        func(a, b int64) int64 { return a + b },
		"subtract":   func(a, b int64) int64 { return a - b },
	}
}

func (d *DictionariesUI) RegisterRoutes(mux *howdah.PageMux) {
	mux.HandleFunc("GET /{$}", d.listPage)
	mux.HandleFunc("GET /dictionaries/{language}/{$}", d.languagePage)
	mux.HandleFunc("GET /dictionaries/{language}/entries", d.entriesPage)
	mux.HandleFunc("GET /dictionaries/{language}/new", d.newEntryPage)
	mux.HandleFunc("GET /dictionaries/{language}/{text}", d.entryPage)
	mux.HandleFunc("POST /dictionaries/{language}/_new", d.saveNewEntry)
	mux.HandleFunc("POST /dictionaries/{language}/{text}", d.saveEntry)
	mux.HandleFunc("POST /dictionaries/{language}/{text}/delete", d.deleteEntry)
}

func (d *DictionariesUI) MenuHook(hooks *howdah.MenuHooks) {
	hooks.RegisterHook(func() []howdah.MenuItem {
		return []howdah.MenuItem{
			{
				Title:  howdah.TL("Dictionaries", "Dictionaries"),
				HREF:   "/",
				Weight: 10,
			},
		}
	})
}

// uiEntry bridges spell.CustomEntry to template-compatible field names.
type uiEntry struct {
	Entry          string
	Status         string
	Description    string
	CommonMistakes []string
	Level          string
	Forms          map[string]string
	Updated        string
	UpdatedBy      string
}

func customEntryToUI(e *spell.CustomEntry) uiEntry {
	level := "error"
	if e.Level == spell.CorrectionLevel_LEVEL_SUGGESTION {
		level = "suggestion"
	}

	return uiEntry{
		Entry:          e.Text,
		Status:         e.Status,
		Description:    e.Description,
		CommonMistakes: e.CommonMistakes,
		Level:          level,
		Forms:          e.Forms,
		Updated:        e.Updated,
		UpdatedBy:      e.UpdatedBy,
	}
}

func customEntriesToUI(entries []*spell.CustomEntry) []uiEntry {
	result := make([]uiEntry, len(entries))
	for i, e := range entries {
		result[i] = customEntryToUI(e)
	}

	return result
}

type dictionariesContents struct {
	Languages   []string
	Language    string
	Entries     []uiEntry
	Entry       *uiEntry
	ActiveEntry string
	NewEntry    bool
	Count       int
	Flash       *flashMessage
	CanWrite    bool
	Prefix      string
	Page        int64
	HasMore     bool
}

func (d *DictionariesUI) hasWriteScope(ctx context.Context) bool {
	accessToken, ok := howdah.AccessToken(ctx)
	if !ok {
		return false
	}

	var claims elephantine.JWTClaims

	if err := accessToken.Claims(&claims); err != nil {
		return false
	}

	return claims.HasScope(ScopeSpellcheckWrite)
}

type flashMessage struct {
	Type    string
	Message string
}

// withServiceAuth bridges howdah's OIDC auth context to elephantine's auth
// context so that the spell.Dictionaries service methods can verify scopes.
func (d *DictionariesUI) withServiceAuth(ctx context.Context) (context.Context, error) {
	headers, ok := twirp.HTTPRequestHeaders(ctx)
	if !ok {
		return ctx, nil
	}

	authHeader := headers.Get("Authorization")
	if authHeader == "" {
		return ctx, nil
	}

	info, err := d.authParser.AuthInfoFromHeader(authHeader)
	if err != nil {
		return nil, fmt.Errorf("parse auth header: %w", err)
	}

	return elephantine.SetAuthInfo(ctx, info), nil
}

func twirpErrorToHTTP(err error) error {
	tErr, ok := err.(twirp.Error)
	if !ok {
		return howdah.InternalHTTPError(err)
	}

	status := twirp.ServerHTTPStatusFromErrorCode(tErr.Code())

	return howdah.NewHTTPError(status, "Error", tErr.Msg(), tErr)
}

func (d *DictionariesUI) listPage(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	_, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	http.Redirect(w, r, "/dictionaries/"+d.languages[0]+"/", http.StatusFound)

	return nil, howdah.ErrSkipRender
}

func (d *DictionariesUI) languagePage(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	ctx, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	lang := r.PathValue("language")

	if !slices.Contains(d.languages, lang) {
		return nil, howdah.NewHTTPError(
			http.StatusNotFound,
			"Error", "Unknown language",
			fmt.Errorf("unknown language %q", lang),
		)
	}

	canWrite := d.hasWriteScope(ctx)

	if isHtmx(r) {
		return d.entryListPage(ctx, lang, "", "", 0)
	}

	entries, hasMore, err := d.listEntries(ctx, lang, "", 0)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	return &howdah.Page{
		Template: "dictionaries.html",
		Title:    howdah.TL("Dictionaries", "Dictionaries"),
		Contents: dictionariesContents{
			Languages: d.languages,
			Language:  lang,
			Entries:   entries,
			Count:     len(entries),
			CanWrite:  canWrite,
			HasMore:   hasMore,
		},
	}, nil
}

func (d *DictionariesUI) newEntryPage(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	ctx, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	lang := r.PathValue("language")
	canWrite := d.hasWriteScope(ctx)

	if isHtmx(r) {
		return &howdah.Page{
			Template: "entry_form.html",
			Contents: dictionariesContents{
				Language: lang,
				NewEntry: true,
				CanWrite: canWrite,
			},
		}, nil
	}

	entries, hasMore, err := d.listEntries(ctx, lang, "", 0)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	return &howdah.Page{
		Template: "dictionaries.html",
		Title:    howdah.TL("Dictionaries", "Dictionaries"),
		Contents: dictionariesContents{
			Languages: d.languages,
			Language:  lang,
			Entries:   entries,
			Count:     len(entries),
			NewEntry:  true,
			CanWrite:  canWrite,
			HasMore:   hasMore,
		},
	}, nil
}

func (d *DictionariesUI) entryPage(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	ctx, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	lang := r.PathValue("language")
	text := r.PathValue("text")
	canWrite := d.hasWriteScope(ctx)

	svcCtx, err := d.withServiceAuth(ctx)
	if err != nil {
		return nil, howdah.InternalHTTPError(err)
	}

	res, err := d.dicts.GetEntry(svcCtx, &spell.GetEntryRequest{
		Language: lang,
		Text:     text,
	})
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	entry := customEntryToUI(res.Entry)

	if isHtmx(r) {
		return &howdah.Page{
			Template: "entry_form.html",
			Contents: dictionariesContents{
				Language:    lang,
				Entry:       &entry,
				ActiveEntry: text,
				CanWrite:    canWrite,
			},
		}, nil
	}

	entries, hasMore, err := d.listEntries(ctx, lang, "", 0)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	return &howdah.Page{
		Template: "dictionaries.html",
		Title:    howdah.TLiteral(text + " – Dictionaries"),
		Contents: dictionariesContents{
			Languages:   d.languages,
			Language:    lang,
			Entries:     entries,
			Count:       len(entries),
			Entry:       &entry,
			ActiveEntry: text,
			CanWrite:    canWrite,
			HasMore:     hasMore,
		},
	}, nil
}

func (d *DictionariesUI) saveNewEntry(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (_ *howdah.Page, outErr error) {
	ctx, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	if !d.hasWriteScope(ctx) {
		return nil, howdah.NewHTTPError(
			http.StatusForbidden,
			"MissingScope", "You need the 'spell_write' scope to make changes",
			fmt.Errorf("missing %q scope", ScopeSpellcheckWrite),
		)
	}

	lang := r.PathValue("language")

	err = r.ParseForm()
	if err != nil {
		return nil, howdah.NewHTTPError(
			http.StatusBadRequest, "Error", "Invalid form data",
			fmt.Errorf("parse form: %w", err),
		)
	}

	text := strings.TrimSpace(r.FormValue("text"))
	if text == "" {
		return &howdah.Page{
			Template: "entry_form.html",
			Contents: dictionariesContents{
				Language: lang,
				NewEntry: true,
				CanWrite: true,
				Flash: &flashMessage{
					Type:    "error",
					Message: "Text is required",
				},
			},
		}, nil
	}

	svcCtx, err := d.withServiceAuth(ctx)
	if err != nil {
		return nil, howdah.InternalHTTPError(err)
	}

	err = d.setEntryFromForm(svcCtx, lang, text, r)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	w.Header().Set("HX-Push-Url", "/dictionaries/"+lang+"/"+url.PathEscape(text))

	res, err := d.dicts.GetEntry(svcCtx, &spell.GetEntryRequest{
		Language: lang,
		Text:     text,
	})
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	entry := customEntryToUI(res.Entry)

	return &howdah.Page{
		Template: "entry_form.html",
		Contents: dictionariesContents{
			Language:    lang,
			Entry:       &entry,
			ActiveEntry: text,
			CanWrite:    true,
			Flash: &flashMessage{
				Type:    "success",
				Message: "Entry created",
			},
		},
	}, nil
}

func (d *DictionariesUI) saveEntry(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (_ *howdah.Page, outErr error) {
	ctx, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	if !d.hasWriteScope(ctx) {
		return nil, howdah.NewHTTPError(
			http.StatusForbidden,
			"MissingScope", "You need the 'spell_write' scope to make changes",
			fmt.Errorf("missing %q scope", ScopeSpellcheckWrite),
		)
	}

	lang := r.PathValue("language")
	text := r.PathValue("text")

	err = r.ParseForm()
	if err != nil {
		return nil, howdah.NewHTTPError(
			http.StatusBadRequest, "Error", "Invalid form data",
			fmt.Errorf("parse form: %w", err),
		)
	}

	svcCtx, err := d.withServiceAuth(ctx)
	if err != nil {
		return nil, howdah.InternalHTTPError(err)
	}

	err = d.setEntryFromForm(svcCtx, lang, text, r)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	res, err := d.dicts.GetEntry(svcCtx, &spell.GetEntryRequest{
		Language: lang,
		Text:     text,
	})
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	entry := customEntryToUI(res.Entry)

	return &howdah.Page{
		Template: "entry_form.html",
		Contents: dictionariesContents{
			Language:    lang,
			Entry:       &entry,
			ActiveEntry: text,
			CanWrite:    true,
			Flash: &flashMessage{
				Type:    "success",
				Message: "Entry updated",
			},
		},
	}, nil
}

func (d *DictionariesUI) deleteEntry(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (_ *howdah.Page, outErr error) {
	ctx, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	if !d.hasWriteScope(ctx) {
		return nil, howdah.NewHTTPError(
			http.StatusForbidden,
			"MissingScope", "You need the 'spell_write' scope to make changes",
			fmt.Errorf("missing %q scope", ScopeSpellcheckWrite),
		)
	}

	lang := r.PathValue("language")
	text := r.PathValue("text")

	svcCtx, err := d.withServiceAuth(ctx)
	if err != nil {
		return nil, howdah.InternalHTTPError(err)
	}

	_, err = d.dicts.DeleteEntry(svcCtx, &spell.DeleteEntryRequest{
		Language: lang,
		Text:     text,
	})
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	w.Header().Set("HX-Redirect", "/dictionaries/"+lang+"/")

	return nil, howdah.ErrSkipRender
}

func (d *DictionariesUI) setEntryFromForm(
	ctx context.Context, lang, text string, r *http.Request,
) error {
	status := strings.TrimSpace(r.FormValue("status"))
	description := strings.TrimSpace(r.FormValue("description"))

	level := spell.CorrectionLevel_LEVEL_ERROR

	if r.FormValue("level") == "suggestion" {
		level = spell.CorrectionLevel_LEVEL_SUGGESTION
	}

	var commonMistakes []string

	cmRaw := strings.TrimSpace(r.FormValue("common_mistakes"))
	if cmRaw != "" {
		for _, line := range strings.Split(cmRaw, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				commonMistakes = append(commonMistakes, line)
			}
		}
	}

	forms := make(map[string]string)

	formsRaw := strings.TrimSpace(r.FormValue("forms"))
	if formsRaw != "" {
		for _, line := range strings.Split(formsRaw, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}

			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}

			forms[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}

	_, err := d.dicts.SetEntry(ctx, &spell.SetEntryRequest{
		Entry: &spell.CustomEntry{
			Language:       lang,
			Text:           text,
			Status:         status,
			Description:    description,
			CommonMistakes: commonMistakes,
			Level:          level,
			Forms:          forms,
		},
	})
	if err != nil {
		return fmt.Errorf("set entry: %w", err)
	}

	return nil
}

func (d *DictionariesUI) listEntries(
	ctx context.Context, lang, prefix string, page int64,
) ([]uiEntry, bool, error) {
	svcCtx, err := d.withServiceAuth(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("bridge auth for list entries: %w", err)
	}

	res, err := d.dicts.ListEntries(svcCtx, &spell.ListEntriesRequest{
		Language: lang,
		Prefix:   prefix,
		Page:     page,
	})
	if err != nil {
		return nil, false, fmt.Errorf("list entries for %q page %d: %w",
			lang, page, err)
	}

	hasMore := len(res.Entries) == 100

	return customEntriesToUI(res.Entries), hasMore, nil
}

func (d *DictionariesUI) entriesPage(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	ctx, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	lang := r.PathValue("language")
	prefix := r.URL.Query().Get("prefix")

	var page int64

	if p := r.URL.Query().Get("page"); p != "" {
		page, err = strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, howdah.NewHTTPError(
				http.StatusBadRequest, "Error", "Invalid page number",
				fmt.Errorf("parse page parameter: %w", err),
			)
		}
	}

	return d.entryListPage(ctx, lang, "", prefix, page)
}

func (d *DictionariesUI) entryListPage(
	ctx context.Context, lang, activeEntry, prefix string, page int64,
) (*howdah.Page, error) {
	entries, hasMore, err := d.listEntries(ctx, lang, prefix, page)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	return &howdah.Page{
		Template: "entry_list.html",
		Contents: dictionariesContents{
			Language:    lang,
			Entries:     entries,
			Count:       len(entries),
			ActiveEntry: activeEntry,
			Prefix:      prefix,
			Page:        page,
			HasMore:     hasMore,
		},
	}, nil
}

func isHtmx(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}
