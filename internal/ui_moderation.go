package internal

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"

	"github.com/ttab/elephant-api/spell"
	"github.com/ttab/howdah"
)

// moderationPageSize is how many pending items the moderation queue shows per
// page.
const moderationPageSize = 10

// moderationFetch bounds how many pending items of each kind are pulled per
// language before paging in memory. A moderation backlog is small, so this is
// comfortably above any realistic queue.
const moderationFetch = 500

// moderationLang is a language together with its count of pending items (words
// and rules). It drives the at-a-glance queue badges and the language switcher.
type moderationLang struct {
	Code    string
	Pending int64
	Active  bool
}

// moderationItem is one pending item in the unified queue, either a dictionary
// word or a pattern rule.
type moderationItem struct {
	Kind           string // "word" or "rule"
	Ident          string // entry text, or rule id, used in action URLs
	Name           string // entry text or rule label
	Level          string
	Description    string
	Updated        string
	UpdatedBy      string
	CommonMistakes []string          // word only
	Forms          map[string]string // word only
	Pattern        string            // rule only
	Replacement    string            // rule only
}

func (m moderationItem) IsRule() bool {
	return m.Kind == "rule"
}

type moderationContents struct {
	Languages []moderationLang
	Language  string
	Items     []moderationItem
	Page      int64
	HasMore   bool
	CanWrite  bool
}

func levelString(l spell.CorrectionLevel) string {
	if l == spell.CorrectionLevel_LEVEL_SUGGESTION {
		return "suggestion"
	}

	return "error"
}

// moderationData assembles the unified moderation view for a language and page:
// the per-language pending badges plus the current page of pending words and
// rules, sorted newest first.
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

	langs := make([]moderationLang, 0, len(d.languages))
	pendingByLang := make(map[string]int64)

	for _, dict := range dicts.Dictionaries {
		pendingByLang[dict.Language] = dict.PendingCount + dict.RulePendingCount
	}

	for _, code := range d.languages {
		langs = append(langs, moderationLang{
			Code:    code,
			Pending: pendingByLang[code],
			Active:  code == lang,
		})
	}

	entries, err := d.dicts.ListEntries(svcCtx, &spell.ListEntriesRequest{
		Language: lang,
		Status:   "pending",
		PageSize: moderationFetch,
	})
	if err != nil {
		return moderationContents{}, fmt.Errorf("list pending entries: %w", err)
	}

	rules, err := d.rules.ListRules(svcCtx, &spell.ListRulesRequest{
		Language: lang,
		Status:   "pending",
		PageSize: moderationFetch,
	})
	if err != nil {
		return moderationContents{}, fmt.Errorf("list pending rules: %w", err)
	}

	var items []moderationItem

	for _, e := range entries.Entries {
		items = append(items, moderationItem{
			Kind:           "word",
			Ident:          e.Text,
			Name:           e.Text,
			Level:          levelString(e.Level),
			Description:    e.Description,
			Updated:        e.Updated,
			UpdatedBy:      e.UpdatedBy,
			CommonMistakes: e.CommonMistakes,
			Forms:          e.Forms,
		})
	}

	for _, r := range rules.Rules {
		items = append(items, moderationItem{
			Kind:        "rule",
			Ident:       strconv.FormatInt(r.Id, 10),
			Name:        r.Name,
			Level:       levelString(r.Level),
			Description: r.Description,
			Updated:     r.Updated,
			UpdatedBy:   r.UpdatedBy,
			Pattern:     r.Pattern,
			Replacement: r.Replacement,
		})
	}

	// Newest first; RFC3339 timestamps sort lexically.
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].Updated > items[j].Updated
	})

	start := int(page) * moderationPageSize
	if start > len(items) {
		start = len(items)
	}

	end := start + moderationPageSize
	if end > len(items) {
		end = len(items)
	}

	return moderationContents{
		Languages: langs,
		Language:  lang,
		Items:     items[start:end],
		Page:      page,
		HasMore:   end < len(items),
		CanWrite:  hasWriteScope(ctx),
	}, nil
}

// moderationRedirect sends /moderation/ to the editor's preferred language.
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
// it returns just the main partial, which the pagination links also target.
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

// moderationAccept marks a pending word or rule as accepted.
func (d *DictionariesUI) moderationAccept(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	return d.moderate(ctx, w, r,
		func(svcCtx context.Context, kind, lang, ident string) error {
			if kind == "rule" {
				id, err := strconv.ParseInt(ident, 10, 64)
				if err != nil {
					return err
				}

				_, err = d.rules.SetRuleStatus(svcCtx,
					&spell.SetRuleStatusRequest{Id: id, Status: "accepted"})

				return err
			}

			_, err := d.dicts.SetEntryStatus(svcCtx,
				&spell.SetEntryStatusRequest{
					Language: lang, Text: ident, Status: "accepted",
				})

			return err
		})
}

// moderationReject removes a pending word or rule.
func (d *DictionariesUI) moderationReject(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	return d.moderate(ctx, w, r,
		func(svcCtx context.Context, kind, lang, ident string) error {
			if kind == "rule" {
				id, err := strconv.ParseInt(ident, 10, 64)
				if err != nil {
					return err
				}

				_, err = d.rules.DeleteRule(svcCtx,
					&spell.DeleteRuleRequest{Id: id})

				return err
			}

			_, err := d.dicts.DeleteEntry(svcCtx, &spell.DeleteEntryRequest{
				Language: lang, Text: ident,
			})

			return err
		})
}

// moderate runs a moderation action against the right store based on the item
// kind, then re-renders the queue. If the action emptied the current page it
// steps back one page.
func (d *DictionariesUI) moderate(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
	action func(svcCtx context.Context, kind, lang, ident string) error,
) (*howdah.Page, error) {
	ctx, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	if !hasWriteScope(ctx) {
		return nil, forbiddenScope()
	}

	lang := r.PathValue("language")
	kind := r.PathValue("kind")
	ident := r.PathValue("ident")

	page, err := pageParam(r)
	if err != nil {
		return nil, err
	}

	svcCtx, err := bridgeServiceAuth(ctx, d.authParser)
	if err != nil {
		return nil, howdah.InternalHTTPError(err)
	}

	err = action(svcCtx, kind, lang, ident)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	contents, err := d.moderationData(ctx, lang, page)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	if len(contents.Items) == 0 && page > 0 {
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
