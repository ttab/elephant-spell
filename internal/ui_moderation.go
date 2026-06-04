package internal

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/ttab/elephant-api/spell"
	"github.com/ttab/howdah"
)

// moderationPageSize is how many pending entries the moderation queue shows per
// page.
const moderationPageSize = 10

// moderationLang is a language together with its count of pending entries. It
// drives the at-a-glance queue badges and the language switcher in the
// moderation view.
type moderationLang struct {
	Code    string
	Pending int64
	Active  bool
}

type moderationContents struct {
	Languages []moderationLang
	Language  string
	Entries   []uiEntry
	Page      int64
	HasMore   bool
	Pending   int64
	CanWrite  bool
}

// moderationData assembles the moderation view for a language and page: the
// per-language pending badges plus the current page of pending entries.
func (d *DictionariesUI) moderationData(
	ctx context.Context, lang string, page int64,
) (moderationContents, error) {
	svcCtx, err := bridgeServiceAuth(ctx, d.authParser)
	if err != nil {
		return moderationContents{}, fmt.Errorf("bridge auth: %w", err)
	}

	dicts, err := d.dicts.ListDictionaries(
		svcCtx, &spell.ListDictionariesRequest{})
	if err != nil {
		return moderationContents{}, fmt.Errorf("list dictionaries: %w", err)
	}

	pendingByLang := make(map[string]int64, len(dicts.Dictionaries))
	for _, dict := range dicts.Dictionaries {
		pendingByLang[dict.Language] = dict.PendingCount
	}

	langs := make([]moderationLang, 0, len(d.languages))
	for _, code := range d.languages {
		langs = append(langs, moderationLang{
			Code:    code,
			Pending: pendingByLang[code],
			Active:  code == lang,
		})
	}

	res, err := d.dicts.ListEntries(svcCtx, &spell.ListEntriesRequest{
		Language: lang,
		Status:   "pending",
		Page:     page,
		PageSize: moderationPageSize,
	})
	if err != nil {
		return moderationContents{}, fmt.Errorf("list pending entries: %w", err)
	}

	return moderationContents{
		Languages: langs,
		Language:  lang,
		Entries:   customEntriesToUI(res.Entries),
		Page:      page,
		HasMore:   int64(len(res.Entries)) == moderationPageSize,
		Pending:   pendingByLang[lang],
		CanWrite:  hasWriteScope(ctx),
	}, nil
}

// moderationRedirect sends /moderation/ to the editor's preferred language,
// reusing the same language cookie as the dictionaries view.
func (d *DictionariesUI) moderationRedirect(
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

	http.Redirect(w, r, "/moderation/"+lang+"/", http.StatusFound)

	return nil, howdah.ErrSkipRender
}

// moderationPage renders the moderation queue for a language. On htmx requests
// it returns just the main partial, which is also what the pagination links
// target.
func (d *DictionariesUI) moderationPage(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	ctx, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	lang := r.PathValue("language")

	if !slices.Contains(d.languages, lang) {
		return nil, howdah.NewHTTPError(
			http.StatusNotFound, "Error", "Unknown language",
			fmt.Errorf("unknown language %q", lang),
		)
	}

	page, err := pageParam(r)
	if err != nil {
		return nil, err
	}

	contents, err := d.moderationData(ctx, lang, page)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	if isHtmx(r) {
		return &howdah.Page{
			Template: "moderation_main.html",
			Contents: contents,
		}, nil
	}

	return &howdah.Page{
		Template:   "moderation.html",
		Title:      howdah.TL("Moderation", "Moderation"),
		Breadcrumb: []howdah.Link{{HREF: "/moderation/"}},
		Contents:   contents,
	}, nil
}

// moderationAccept marks a pending entry as accepted.
func (d *DictionariesUI) moderationAccept(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	return d.moderate(ctx, w, r, func(svcCtx context.Context, lang, text string) error {
		_, err := d.dicts.SetEntryStatus(svcCtx, &spell.SetEntryStatusRequest{
			Language: lang,
			Text:     text,
			Status:   "accepted",
		})

		return err
	})
}

// moderationReject removes a pending entry.
func (d *DictionariesUI) moderationReject(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	return d.moderate(ctx, w, r, func(svcCtx context.Context, lang, text string) error {
		_, err := d.dicts.DeleteEntry(svcCtx, &spell.DeleteEntryRequest{
			Language: lang,
			Text:     text,
		})

		return err
	})
}

// moderate runs a moderation action and re-renders the queue. If the action
// emptied the current page it steps back one page so the moderator isn't left
// on a blank page.
func (d *DictionariesUI) moderate(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
	action func(svcCtx context.Context, lang, text string) error,
) (*howdah.Page, error) {
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

	page, err := pageParam(r)
	if err != nil {
		return nil, err
	}

	svcCtx, err := bridgeServiceAuth(ctx, d.authParser)
	if err != nil {
		return nil, howdah.InternalHTTPError(err)
	}

	err = action(svcCtx, lang, text)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	contents, err := d.moderationData(ctx, lang, page)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	if len(contents.Entries) == 0 && page > 0 {
		contents, err = d.moderationData(ctx, lang, page-1)
		if err != nil {
			return nil, twirpErrorToHTTP(err)
		}
	}

	return &howdah.Page{
		Template: "moderation_main.html",
		Contents: contents,
	}, nil
}

// pageParam reads the optional "page" query parameter.
func pageParam(r *http.Request) (int64, error) {
	p := r.URL.Query().Get("page")
	if p == "" {
		return 0, nil
	}

	page, err := strconv.ParseInt(p, 10, 64)
	if err != nil {
		return 0, howdah.NewHTTPError(
			http.StatusBadRequest, "Error", "Invalid page number",
			fmt.Errorf("parse page parameter: %w", err),
		)
	}

	return page, nil
}
