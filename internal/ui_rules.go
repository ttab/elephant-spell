package internal

import (
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/ttab/elephant-api/spell"
	"github.com/ttab/elephantine"
	"github.com/ttab/howdah"
)

// RulesUI is the web UI for managing pattern rules, a separate section from the
// dictionary words.
type RulesUI struct {
	log             *slog.Logger
	auth            howdah.Authenticator
	authParser      elephantine.AuthInfoParser
	rules           spell.Rules
	dicts           spell.Dictionaries
	languages       []string
	defaultLanguage string
}

func NewRulesUI(
	logger *slog.Logger,
	auth howdah.Authenticator,
	authParser elephantine.AuthInfoParser,
	rules spell.Rules,
	dicts spell.Dictionaries,
	languages []string,
	defaultLanguage string,
) *RulesUI {
	languages = slices.Clone(languages)
	slices.Sort(languages)

	if defaultLanguage == "" {
		defaultLanguage = languages[0]
	}

	return &RulesUI{
		log:             logger,
		auth:            auth,
		authParser:      authParser,
		rules:           rules,
		dicts:           dicts,
		languages:       languages,
		defaultLanguage: defaultLanguage,
	}
}

func (d *RulesUI) GetTemplateFuncs() template.FuncMap {
	return template.FuncMap{}
}

func (d *RulesUI) RegisterRoutes(mux *howdah.PageMux) {
	mux.HandleFunc("GET /rules/{$}", d.redirectPage)
	mux.HandleFunc("GET /rules/{language}/{$}", d.languagePage)
	mux.HandleFunc("GET /rules/{language}/list", d.listPartial)
	mux.HandleFunc("GET /rules/{language}/new", d.newRulePage)
	mux.HandleFunc("GET /rules/{language}/{name}", d.rulePage)
	mux.HandleFunc("POST /rules/{language}/_new", d.saveNewRule)
	mux.HandleFunc("POST /rules/{language}/{name}", d.saveRule)
	mux.HandleFunc("POST /rules/{language}/{name}/delete", d.deleteRule)
}

func (d *RulesUI) MenuHook(hooks *howdah.MenuHooks) {
	hooks.RegisterHook(func() []howdah.MenuItem {
		return []howdah.MenuItem{
			{
				Title:  howdah.TL("Rules", "Rules"),
				HREF:   "/rules/",
				Weight: 15,
			},
		}
	})
}

// uiRule mirrors a rule for templates, with guard lists flattened to
// comma-separated strings for the text inputs.
type uiRule struct {
	Name        string
	Status      string
	Description string
	Level       string
	Pattern     string
	Replacement string
	Before      string
	After       string
	NotBefore   string
	NotAfter    string
	Updated     string
	UpdatedBy   string
}

func ruleToUI(r *spell.Rule) uiRule {
	level := "error"
	if r.Level == spell.CorrectionLevel_LEVEL_SUGGESTION {
		level = "suggestion"
	}

	return uiRule{
		Name:        r.Name,
		Status:      r.Status,
		Description: r.Description,
		Level:       level,
		Pattern:     r.Pattern,
		Replacement: r.Replacement,
		Before:      strings.Join(r.Before, ", "),
		After:       strings.Join(r.After, ", "),
		NotBefore:   strings.Join(r.NotBefore, ", "),
		NotAfter:    strings.Join(r.NotAfter, ", "),
		Updated:     r.Updated,
		UpdatedBy:   r.UpdatedBy,
	}
}

type rulesContents struct {
	Languages  []string
	Language   string
	Rules      []uiRule
	Rule       *uiRule
	ActiveRule string
	NewRule    bool
	Count      int
	Flash      *flashMessage
	CanWrite   bool
	Query      string
	Page       int64
	HasMore    bool
}

func (d *RulesUI) redirectPage(
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

	http.Redirect(w, r, "/rules/"+lang+"/", http.StatusFound)

	return nil, howdah.ErrSkipRender
}

func (d *RulesUI) languagePage(
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

	rules, hasMore, err := d.listRules(ctx, lang, "", 0)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	return &howdah.Page{
		Template: "rules.html",
		Title:    howdah.TL("Rules", "Rules"),
		Contents: rulesContents{
			Languages: d.languages,
			Language:  lang,
			Rules:     rules,
			Count:     d.ruleCount(ctx, lang),
			CanWrite:  hasWriteScope(ctx),
			HasMore:   hasMore,
		},
	}, nil
}

func (d *RulesUI) listPartial(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	ctx, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	lang := r.PathValue("language")
	query := r.URL.Query().Get("query")

	page, err := pageParam(r)
	if err != nil {
		return nil, err
	}

	rules, hasMore, err := d.listRules(ctx, lang, query, page)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	return &howdah.Page{
		Template: "rule_list.html",
		Contents: rulesContents{
			Language: lang,
			Rules:    rules,
			Query:    query,
			Page:     page,
			HasMore:  hasMore,
		},
	}, nil
}

func (d *RulesUI) newRulePage(
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
			Template: "rule_form.html",
			Contents: rulesContents{Language: lang, NewRule: true, CanWrite: canWrite},
		}, nil
	}

	rules, hasMore, err := d.listRules(ctx, lang, "", 0)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	return &howdah.Page{
		Template: "rules.html",
		Title:    howdah.TL("Rules", "Rules"),
		Contents: rulesContents{
			Languages: d.languages,
			Language:  lang,
			Rules:     rules,
			Count:     d.ruleCount(ctx, lang),
			NewRule:   true,
			CanWrite:  canWrite,
			HasMore:   hasMore,
		},
	}, nil
}

func (d *RulesUI) rulePage(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	ctx, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	lang := r.PathValue("language")
	name := r.PathValue("name")
	canWrite := hasWriteScope(ctx)

	svcCtx, err := bridgeServiceAuth(ctx, d.authParser)
	if err != nil {
		return nil, howdah.InternalHTTPError(err)
	}

	res, err := d.rules.GetRule(svcCtx, &spell.GetRuleRequest{
		Language: lang,
		Name:     name,
	})
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	rule := ruleToUI(res.Rule)

	if isHtmx(r) {
		return &howdah.Page{
			Template: "rule_form.html",
			Contents: rulesContents{
				Language: lang, Rule: &rule, ActiveRule: name, CanWrite: canWrite,
			},
		}, nil
	}

	rules, hasMore, err := d.listRules(ctx, lang, "", 0)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	return &howdah.Page{
		Template: "rules.html",
		Title:    howdah.TLiteral(name + " – Rules"),
		Contents: rulesContents{
			Languages:  d.languages,
			Language:   lang,
			Rules:      rules,
			Count:      d.ruleCount(ctx, lang),
			Rule:       &rule,
			ActiveRule: name,
			CanWrite:   canWrite,
			HasMore:    hasMore,
		},
	}, nil
}

func (d *RulesUI) saveNewRule(
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

	err = r.ParseForm()
	if err != nil {
		return nil, howdah.NewHTTPError(
			http.StatusBadRequest, "Error", "Invalid form data",
			fmt.Errorf("parse form: %w", err))
	}

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		return &howdah.Page{
			Template: "rule_form.html",
			Contents: rulesContents{
				Language: lang, NewRule: true, CanWrite: true,
				Flash: &flashMessage{
					Type:    "error",
					Message: howdah.TL("NameRequired", "Name is required"),
				},
			},
		}, nil
	}

	svcCtx, err := bridgeServiceAuth(ctx, d.authParser)
	if err != nil {
		return nil, howdah.InternalHTTPError(err)
	}

	err = d.setRuleFromForm(svcCtx, lang, name, r)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	w.Header().Set("HX-Push-Url", "/rules/"+lang+"/"+url.PathEscape(name))

	return d.ruleFormResponse(svcCtx, lang, name,
		howdah.TL("RuleCreated", "Rule created"))
}

func (d *RulesUI) saveRule(
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
	name := r.PathValue("name")

	err = r.ParseForm()
	if err != nil {
		return nil, howdah.NewHTTPError(
			http.StatusBadRequest, "Error", "Invalid form data",
			fmt.Errorf("parse form: %w", err))
	}

	svcCtx, err := bridgeServiceAuth(ctx, d.authParser)
	if err != nil {
		return nil, howdah.InternalHTTPError(err)
	}

	err = d.setRuleFromForm(svcCtx, lang, name, r)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	return d.ruleFormResponse(svcCtx, lang, name,
		howdah.TL("RuleUpdated", "Rule updated"))
}

func (d *RulesUI) deleteRule(
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
	name := r.PathValue("name")

	svcCtx, err := bridgeServiceAuth(ctx, d.authParser)
	if err != nil {
		return nil, howdah.InternalHTTPError(err)
	}

	_, err = d.rules.DeleteRule(svcCtx, &spell.DeleteRuleRequest{
		Language: lang,
		Name:     name,
	})
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	w.Header().Set("HX-Redirect", "/rules/"+lang+"/")

	return nil, howdah.ErrSkipRender
}

// ruleFormResponse re-reads a rule and renders the form with a flash message.
func (d *RulesUI) ruleFormResponse(
	svcCtx context.Context, lang, name string, flash howdah.TextLabel,
) (*howdah.Page, error) {
	res, err := d.rules.GetRule(svcCtx, &spell.GetRuleRequest{
		Language: lang,
		Name:     name,
	})
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	rule := ruleToUI(res.Rule)

	return &howdah.Page{
		Template: "rule_form.html",
		Contents: rulesContents{
			Language:   lang,
			Rule:       &rule,
			ActiveRule: name,
			CanWrite:   true,
			Flash:      &flashMessage{Type: "success", Message: flash},
		},
	}, nil
}

func (d *RulesUI) setRuleFromForm(
	ctx context.Context, lang, name string, r *http.Request,
) error {
	level := spell.CorrectionLevel_LEVEL_ERROR
	if r.FormValue("level") == "suggestion" {
		level = spell.CorrectionLevel_LEVEL_SUGGESTION
	}

	status := strings.TrimSpace(r.FormValue("status"))
	if status == "" {
		status = "accepted"
	}

	_, err := d.rules.SetRule(ctx, &spell.SetRuleRequest{
		Rule: &spell.Rule{
			Language:    lang,
			Name:        name,
			Status:      status,
			Description: strings.TrimSpace(r.FormValue("description")),
			Level:       level,
			Pattern:     strings.TrimSpace(r.FormValue("pattern")),
			Replacement: strings.TrimSpace(r.FormValue("replacement")),
			Before:      splitCommaList(r.FormValue("before")),
			After:       splitCommaList(r.FormValue("after")),
			NotBefore:   splitCommaList(r.FormValue("not_before")),
			NotAfter:    splitCommaList(r.FormValue("not_after")),
		},
	})
	if err != nil {
		return fmt.Errorf("set rule: %w", err)
	}

	return nil
}

func (d *RulesUI) listRules(
	ctx context.Context, lang, query string, page int64,
) ([]uiRule, bool, error) {
	svcCtx, err := bridgeServiceAuth(ctx, d.authParser)
	if err != nil {
		return nil, false, fmt.Errorf("bridge auth for list rules: %w", err)
	}

	res, err := d.rules.ListRules(svcCtx, &spell.ListRulesRequest{
		Language: lang,
		Query:    query,
		Page:     page,
	})
	if err != nil {
		return nil, false, fmt.Errorf("list rules for %q page %d: %w",
			lang, page, err)
	}

	rules := make([]uiRule, len(res.Rules))
	for i, r := range res.Rules {
		rules[i] = ruleToUI(r)
	}

	return rules, len(res.Rules) == 100, nil
}

func (d *RulesUI) ruleCount(ctx context.Context, lang string) int {
	svcCtx, err := bridgeServiceAuth(ctx, d.authParser)
	if err != nil {
		return 0
	}

	res, err := d.dicts.ListDictionaries(svcCtx, &spell.ListDictionariesRequest{})
	if err != nil {
		return 0
	}

	for _, dict := range res.Dictionaries {
		if dict.Language == lang {
			return int(dict.RuleCount)
		}
	}

	return 0
}

// splitCommaList splits a comma-separated input into trimmed, non-empty values.
func splitCommaList(s string) []string {
	var out []string

	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}

	return out
}

func forbiddenScope() error {
	return howdah.NewHTTPError(
		http.StatusForbidden,
		"MissingScope", "You need the 'spell_write' scope to make changes",
		fmt.Errorf("missing %q scope", ScopeSpellcheckWrite),
	)
}
