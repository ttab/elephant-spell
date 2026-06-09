package internal

import (
	"context"
	"errors"
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
	log             *slog.Logger
	auth            howdah.Authenticator
	authParser      elephantine.AuthInfoParser
	dicts           spell.Dictionaries
	rules           spell.Rules
	languages       []string
	defaultLanguage string
}

func NewDictionariesUI(
	logger *slog.Logger,
	auth howdah.Authenticator,
	authParser elephantine.AuthInfoParser,
	dicts spell.Dictionaries,
	rules spell.Rules,
	languages []string,
	defaultLanguage string,
) *DictionariesUI {
	slices.Sort(languages)

	if defaultLanguage == "" {
		defaultLanguage = languages[0]
	}

	return &DictionariesUI{
		log:             logger,
		auth:            auth,
		authParser:      authParser,
		dicts:           dicts,
		rules:           rules,
		languages:       languages,
		defaultLanguage: defaultLanguage,
	}
}

func (d *DictionariesUI) GetTemplateFuncs() template.FuncMap {
	return template.FuncMap{
		"pathEscape":    url.PathEscape,
		"add":           func(a, b int64) int64 { return a + b },
		"subtract":      func(a, b int64) int64 { return a - b },
		"expandPreview": mistakesPreview,
	}
}

func (d *DictionariesUI) RegisterRoutes(mux *howdah.PageMux) {
	mux.HandleFunc("GET /{$}", d.listPage)
	mux.HandleFunc("GET /api/keepalive", d.keepalivePage)
	mux.HandleFunc("GET /dictionaries/{language}/{$}", d.languagePage)
	mux.HandleFunc("GET /dictionaries/{language}/entries", d.entriesPage)
	mux.HandleFunc("GET /dictionaries/{language}/new", d.newEntryPage)
	mux.HandleFunc("GET /dictionaries/{language}/{text}", d.entryPage)
	mux.HandleFunc("POST /dictionaries/{language}/_new", d.saveNewEntry)
	mux.HandleFunc("POST /dictionaries/{language}/_validate", d.validateMistakes)
	mux.HandleFunc("POST /dictionaries/{language}/_expansions", d.listExpansions)
	mux.HandleFunc("POST /dictionaries/{language}/{text}", d.saveEntry)
	mux.HandleFunc("POST /dictionaries/{language}/{text}/delete", d.deleteEntry)
	mux.HandleFunc("GET /dictionaries/{language}/{text}/rename", d.renameEntryForm)
	mux.HandleFunc("POST /dictionaries/{language}/{text}/rename", d.renameEntry)
	mux.HandleFunc("GET /moderation/{$}", d.moderationRedirect)
	mux.HandleFunc("GET /moderation/{language}/{$}", d.moderationPage)
	mux.HandleFunc("POST /moderation/{language}/{kind}/{ident}/accept", d.moderationAccept)
	mux.HandleFunc("POST /moderation/{language}/{kind}/{ident}/reject", d.moderationReject)
}

func (d *DictionariesUI) MenuHook(hooks *howdah.MenuHooks) {
	hooks.RegisterHook(func() []howdah.MenuItem {
		return []howdah.MenuItem{
			{
				Title:  howdah.TL("Dictionaries", "Dictionaries"),
				HREF:   "/",
				Weight: 10,
			},
			{
				Title:  howdah.TL("Moderation", "Moderation"),
				HREF:   "/moderation/",
				Weight: 20,
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
	CaseSensitive  bool
	Before         string
	After          string
	NotBefore      string
	NotAfter       string
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
		CaseSensitive:  e.CaseSensitive,
		Before:         strings.Join(e.Before, ", "),
		After:          strings.Join(e.After, ", "),
		NotBefore:      strings.Join(e.NotBefore, ", "),
		NotAfter:       strings.Join(e.NotAfter, ", "),
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
	Rename      bool
	Count       int
	Flash       *flashMessage
	CanWrite    bool
	Query       string
	Page        int64
	HasMore     bool
}

func (d *DictionariesUI) entryCount(ctx context.Context, lang string) (int, error) {
	svcCtx, err := bridgeServiceAuth(ctx, d.authParser)
	if err != nil {
		return 0, fmt.Errorf("bridge auth for entry count: %w", err)
	}

	res, err := d.dicts.ListDictionaries(svcCtx, &spell.ListDictionariesRequest{})
	if err != nil {
		return 0, fmt.Errorf("list dictionaries: %w", err)
	}

	for _, dict := range res.Dictionaries {
		if dict.Language == lang {
			return int(dict.EntryCount), nil
		}
	}

	return 0, nil
}

func hasWriteScope(ctx context.Context) bool {
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
	Message howdah.TextLabel
}

// statusOption is a selectable entry/rule status for the editor's status
// dropdown.
type statusOption struct {
	Value    string
	Label    howdah.TextLabel
	Selected bool
}

// statusOptions builds the status dropdown choices, marking the current status
// as selected. New entries and rules (empty current status) default to
// "pending" so additions go through moderation before taking effect.
func statusOptions(current string) []statusOption {
	if current == "" {
		current = "pending"
	}

	return []statusOption{
		{
			Value:    "accepted",
			Label:    howdah.TL("Accepted", "Accepted"),
			Selected: current == "accepted",
		},
		{
			Value:    "pending",
			Label:    howdah.TL("Pending", "Pending"),
			Selected: current == "pending",
		},
	}
}

// StatusOptions returns the status dropdown choices for the entry editor,
// defaulting to "pending" for new entries.
func (c dictionariesContents) StatusOptions() []statusOption {
	var current string

	if c.Entry != nil {
		current = c.Entry.Status
	}

	return statusOptions(current)
}

// bridgeServiceAuth bridges howdah's OIDC auth context to elephantine's auth
// context so that the spell service methods can verify scopes.
func bridgeServiceAuth(
	ctx context.Context, authParser elephantine.AuthInfoParser,
) (context.Context, error) {
	headers, ok := twirp.HTTPRequestHeaders(ctx)
	if !ok {
		return ctx, nil
	}

	authHeader := headers.Get("Authorization")
	if authHeader == "" {
		return ctx, nil
	}

	info, err := authParser.AuthInfoFromHeader(authHeader)
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

// parseForm parses an HTTP request's form, returning a bad-request HTTP error on
// failure so handlers can surface it directly.
func parseForm(r *http.Request) error {
	err := r.ParseForm()
	if err != nil {
		return howdah.NewHTTPError(
			http.StatusBadRequest, "Error", "Invalid form data",
			fmt.Errorf("parse form: %w", err))
	}

	return nil
}

func (d *DictionariesUI) keepalivePage(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	_, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	w.WriteHeader(http.StatusNoContent)

	return nil, howdah.ErrSkipRender
}

func (d *DictionariesUI) listPage(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	_, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	lang := d.defaultLanguage

	if c, err := r.Cookie("lang"); err == nil && c.Value != "" {
		if match := matchLanguage(d.languages, c.Value); match != "" {
			lang = match
		}
	}

	http.Redirect(w, r, "/dictionaries/"+lang+"/", http.StatusFound)

	return nil, howdah.ErrSkipRender
}

// matchLanguage finds a dictionary language matching the given UI locale code.
// It first tries an exact match, then falls back to prefix matching (e.g. "sv"
// matches "sv-se").
func matchLanguage(languages []string, code string) string {
	code = strings.ToLower(code)

	for _, lang := range languages {
		if lang == code {
			return lang
		}
	}

	for _, lang := range languages {
		if strings.HasPrefix(lang, code+"-") || strings.HasPrefix(code, lang+"-") {
			return lang
		}
	}

	return ""
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

	canWrite := hasWriteScope(ctx)

	if isHtmx(r) {
		return d.entryListPage(ctx, lang, "", "", 0)
	}

	entries, hasMore, err := d.listEntries(ctx, lang, "", 0)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	count, err := d.entryCount(ctx, lang)
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
			Count:     count,
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
	canWrite := hasWriteScope(ctx)

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

	count, err := d.entryCount(ctx, lang)
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
			Count:     count,
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
	canWrite := hasWriteScope(ctx)

	svcCtx, err := bridgeServiceAuth(ctx, d.authParser)
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

	count, err := d.entryCount(ctx, lang)
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
			Count:       count,
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

	if !hasWriteScope(ctx) {
		return nil, howdah.NewHTTPError(
			http.StatusForbidden,
			"MissingScope", "You need the 'spell_write' scope to make changes",
			fmt.Errorf("missing %q scope", ScopeSpellcheckWrite),
		)
	}

	lang := r.PathValue("language")

	if err := parseForm(r); err != nil {
		return nil, err
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
					Message: howdah.TL("TextRequired", "Text is required"),
				},
			},
		}, nil
	}

	svcCtx, err := bridgeServiceAuth(ctx, d.authParser)
	if err != nil {
		return nil, howdah.InternalHTTPError(err)
	}

	err = d.setEntryFromForm(svcCtx, lang, text, r)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	w.Header().Set("HX-Push-Url", "/dictionaries/"+lang+"/"+url.PathEscape(text))

	return d.entryDetailResponse(ctx, svcCtx, lang, text, &flashMessage{
		Type:    "success",
		Message: howdah.TL("EntryCreated", "Entry created"),
	})
}

// entryDetailResponse renders the entry detail (a form, or the empty
// placeholder when text is empty) together with an out-of-band refresh of the
// sidebar list, so the list reflects creates, edits and deletes immediately.
func (d *DictionariesUI) entryDetailResponse(
	ctx, svcCtx context.Context, lang, text string, flash *flashMessage,
) (*howdah.Page, error) {
	entries, hasMore, err := d.listEntries(ctx, lang, "", 0)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	contents := dictionariesContents{
		Language: lang,
		Entries:  entries,
		HasMore:  hasMore,
		CanWrite: true,
		Flash:    flash,
	}

	if text != "" {
		res, err := d.dicts.GetEntry(svcCtx, &spell.GetEntryRequest{
			Language: lang,
			Text:     text,
		})
		if err != nil {
			return nil, twirpErrorToHTTP(err)
		}

		entry := customEntryToUI(res.Entry)
		contents.Entry = &entry
		contents.ActiveEntry = text
	}

	return &howdah.Page{
		Template: "entry_response.html",
		Contents: contents,
	}, nil
}

func (d *DictionariesUI) saveEntry(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (_ *howdah.Page, outErr error) {
	ctx, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	if !hasWriteScope(ctx) {
		return nil, howdah.NewHTTPError(
			http.StatusForbidden,
			"MissingScope", "You need the 'spell_write' scope to make changes",
			fmt.Errorf("missing %q scope", ScopeSpellcheckWrite),
		)
	}

	lang := r.PathValue("language")
	text := r.PathValue("text")

	if err := parseForm(r); err != nil {
		return nil, err
	}

	svcCtx, err := bridgeServiceAuth(ctx, d.authParser)
	if err != nil {
		return nil, howdah.InternalHTTPError(err)
	}

	err = d.setEntryFromForm(svcCtx, lang, text, r)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	return d.entryDetailResponse(ctx, svcCtx, lang, text, &flashMessage{
		Type:    "success",
		Message: howdah.TL("EntryUpdated", "Entry updated"),
	})
}

func (d *DictionariesUI) deleteEntry(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (_ *howdah.Page, outErr error) {
	ctx, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	if !hasWriteScope(ctx) {
		return nil, howdah.NewHTTPError(
			http.StatusForbidden,
			"MissingScope", "You need the 'spell_write' scope to make changes",
			fmt.Errorf("missing %q scope", ScopeSpellcheckWrite),
		)
	}

	lang := r.PathValue("language")
	text := r.PathValue("text")

	svcCtx, err := bridgeServiceAuth(ctx, d.authParser)
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

	w.Header().Set("HX-Push-Url", "/dictionaries/"+lang+"/")

	return d.entryDetailResponse(ctx, svcCtx, lang, "", nil)
}

// renameEntryForm shows the simple rename form for an entry.
func (d *DictionariesUI) renameEntryForm(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	ctx, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	if !hasWriteScope(ctx) {
		return nil, forbiddenScope()
	}

	lang := r.PathValue("language")
	text := r.PathValue("text")

	svcCtx, err := bridgeServiceAuth(ctx, d.authParser)
	if err != nil {
		return nil, howdah.InternalHTTPError(err)
	}

	res, err := d.dicts.GetEntry(svcCtx, &spell.GetEntryRequest{
		Language: lang, Text: text,
	})
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	entry := customEntryToUI(res.Entry)

	if isHtmx(r) {
		return &howdah.Page{
			Template: "entry_rename.html",
			Contents: dictionariesContents{
				Language: lang, Entry: &entry, ActiveEntry: text,
				Rename: true, CanWrite: true,
			},
		}, nil
	}

	entries, hasMore, err := d.listEntries(ctx, lang, "", 0)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	count, err := d.entryCount(ctx, lang)
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
			Count:       count,
			Entry:       &entry,
			ActiveEntry: text,
			Rename:      true,
			CanWrite:    true,
			HasMore:     hasMore,
		},
	}, nil
}

// renameEntry applies a rename and, on success, swaps in the renamed entry's
// detail. On a validation or conflict error it re-renders the rename form with
// a message instead of an error page.
func (d *DictionariesUI) renameEntry(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (_ *howdah.Page, outErr error) {
	ctx, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	if !hasWriteScope(ctx) {
		return nil, forbiddenScope()
	}

	lang := r.PathValue("language")
	text := r.PathValue("text")

	err = r.ParseForm()
	if err != nil {
		return nil, howdah.NewHTTPError(
			http.StatusBadRequest, "Error", "Invalid form data",
			fmt.Errorf("parse form: %w", err))
	}

	newText := strings.TrimSpace(r.FormValue("new_text"))

	svcCtx, err := bridgeServiceAuth(ctx, d.authParser)
	if err != nil {
		return nil, howdah.InternalHTTPError(err)
	}

	if newText == "" || newText == text {
		return d.renameFormWithFlash(svcCtx, lang, text, &flashMessage{
			Type:    "error",
			Message: howdah.TL("RenameUnchanged", "Enter a different text"),
		})
	}

	_, err = d.dicts.RenameEntry(svcCtx, &spell.RenameEntryRequest{
		Language: lang, Text: text, NewText: newText,
	})
	if err != nil {
		return d.renameFormWithFlash(svcCtx, lang, text, &flashMessage{
			Type:    "error",
			Message: renameErrorMessage(err),
		})
	}

	w.Header().Set("HX-Push-Url", "/dictionaries/"+lang+"/"+url.PathEscape(newText))

	return d.entryDetailResponse(ctx, svcCtx, lang, newText, &flashMessage{
		Type:    "success",
		Message: howdah.TL("EntryRenamed", "Entry renamed"),
	})
}

// renameFormWithFlash re-renders the rename form for an entry with a message.
func (d *DictionariesUI) renameFormWithFlash(
	svcCtx context.Context, lang, text string, flash *flashMessage,
) (*howdah.Page, error) {
	res, err := d.dicts.GetEntry(svcCtx, &spell.GetEntryRequest{
		Language: lang, Text: text,
	})
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	entry := customEntryToUI(res.Entry)

	return &howdah.Page{
		Template: "entry_rename.html",
		Contents: dictionariesContents{
			Language: lang, Entry: &entry, ActiveEntry: text,
			Rename: true, CanWrite: true, Flash: flash,
		},
	}, nil
}

// renameErrorMessage maps a rename RPC error to an editor-facing message.
func renameErrorMessage(err error) howdah.TextLabel {
	var twerr twirp.Error
	if errors.As(err, &twerr) {
		switch twerr.Code() {
		case twirp.AlreadyExists:
			return howdah.TL("RenameConflict",
				"An entry with that text already exists")
		case twirp.NotFound:
			return howdah.TL("RenameGone", "The entry no longer exists")
		}
	}

	return howdah.TL("RenameFailed", "Could not rename the entry")
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

	// Forms are submitted as parallel arrays of incorrect/correct inputs, one
	// pair per row in the form editor.
	forms := parseForms(r.Form)

	_, err := d.dicts.SetEntry(ctx, &spell.SetEntryRequest{
		Entry: &spell.CustomEntry{
			Language:       lang,
			Text:           text,
			Status:         status,
			Description:    description,
			CommonMistakes: commonMistakes,
			Level:          level,
			Forms:          forms,
			CaseSensitive:  r.FormValue("case_sensitive") == "on",
			Before:         splitCommaList(r.FormValue("before")),
			After:          splitCommaList(r.FormValue("after")),
			NotBefore:      splitCommaList(r.FormValue("not_before")),
			NotAfter:       splitCommaList(r.FormValue("not_after")),
		},
	})
	if err != nil {
		return fmt.Errorf("set entry: %w", err)
	}

	return nil
}

// patternLine is the validation result for a single common-mistakes line.
type patternLine struct {
	Line  string
	Count int
	Error string
}

type patternPreviewContents struct {
	Results []patternLine
	Total   int
}

// HasExpansions reports whether the preview is worth showing — i.e. some line
// actually expands to more than one combination, or has a brace error. When
// every line is a plain literal the preview would just repeat the text field.
func (p patternPreviewContents) HasExpansions() bool {
	for _, r := range p.Results {
		if r.Count > 1 || r.Error != "" {
			return true
		}
	}

	return false
}

// validateMistakes expands the submitted common-mistakes patterns server-side
// and returns a preview of how many combinations each line yields, plus any
// brace errors. It reuses the canonical Expand logic so the preview can't drift
// from what the spellchecker actually does.
func (d *DictionariesUI) validateMistakes(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	_, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	if err := parseForm(r); err != nil {
		return nil, err
	}

	return &howdah.Page{
		Template: "pattern_preview.html",
		Contents: mistakesPreview(
			strings.Split(r.FormValue("common_mistakes"), "\n")),
	}, nil
}

// mistakesPreview expands each common-mistakes line and reports how many
// combinations it yields, plus any brace errors. Shared by the live validation
// endpoint and the form's at-load preview so they can't drift from what the
// spellchecker actually does.
func mistakesPreview(lines []string) patternPreviewContents {
	var (
		results []patternLine
		total   int
	)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		res := patternLine{Line: line}

		expanded, err := Expand(line)
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Count = len(expanded)
			total += len(expanded)
		}

		results = append(results, res)
	}

	return patternPreviewContents{Results: results, Total: total}
}

// parseForms pairs the parallel forms_incorrect/forms_correct inputs from the
// entry form into an incorrect→correct map, skipping rows where either side is
// blank.
func parseForms(form url.Values) map[string]string {
	forms := make(map[string]string)

	incorrect := form["forms_incorrect"]
	correct := form["forms_correct"]

	for i := range incorrect {
		k := strings.TrimSpace(incorrect[i])
		if k == "" || i >= len(correct) {
			continue
		}

		v := strings.TrimSpace(correct[i])
		if v == "" {
			continue
		}

		forms[k] = v
	}

	return forms
}

// maxExpansionsShown caps how many expansions the modal renders, to keep the
// DOM bounded for patterns that expand to very many combinations.
const maxExpansionsShown = 500

// expansionGroup is the full expansion of one common-mistakes line.
type expansionGroup struct {
	Line       string
	Expansions []string
	Error      string
}

type expansionsContents struct {
	Groups  []expansionGroup
	Total   int
	Shown   int
	Omitted int
}

// listExpansions enumerates every expansion of the submitted common-mistakes
// patterns for the detail modal, reusing the canonical Expand logic.
func (d *DictionariesUI) listExpansions(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	_, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	if err := parseForm(r); err != nil {
		return nil, err
	}

	var (
		groups []expansionGroup
		total  int
		shown  int
	)

	for _, line := range strings.Split(r.FormValue("common_mistakes"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		group := expansionGroup{Line: line}

		expanded, err := Expand(line)
		if err != nil {
			group.Error = err.Error()
			groups = append(groups, group)

			continue
		}

		total += len(expanded)

		for _, e := range expanded {
			if shown >= maxExpansionsShown {
				break
			}

			group.Expansions = append(group.Expansions, e)
			shown++
		}

		groups = append(groups, group)
	}

	return &howdah.Page{
		Template: "expansions.html",
		Contents: expansionsContents{
			Groups:  groups,
			Total:   total,
			Shown:   shown,
			Omitted: total - shown,
		},
	}, nil
}

func (d *DictionariesUI) listEntries(
	ctx context.Context, lang, query string, page int64,
) ([]uiEntry, bool, error) {
	svcCtx, err := bridgeServiceAuth(ctx, d.authParser)
	if err != nil {
		return nil, false, fmt.Errorf("bridge auth for list entries: %w", err)
	}

	res, err := d.dicts.ListEntries(svcCtx, &spell.ListEntriesRequest{
		Language: lang,
		Query:    query,
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
	query := r.URL.Query().Get("query")

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

	return d.entryListPage(ctx, lang, "", query, page)
}

func (d *DictionariesUI) entryListPage(
	ctx context.Context, lang, activeEntry, query string, page int64,
) (*howdah.Page, error) {
	entries, hasMore, err := d.listEntries(ctx, lang, query, page)
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
			Query:       query,
			Page:        page,
			HasMore:     hasMore,
		},
	}, nil
}

func isHtmx(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}
