package internal

import (
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/ttab/elephant-api/spell"
	"github.com/ttab/elephantine"
	"github.com/ttab/howdah"
)

// rulesPageSize is the rule list page size requested from the service. It is
// sent explicitly so the "is there another page?" check (a full page implies
// more) doesn't drift from the service-side default.
const rulesPageSize = 100

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
	mux.HandleFunc("GET /rules/{language}/{id}", d.rulePage)
	mux.HandleFunc("POST /rules/{language}/_new", d.saveNewRule)
	mux.HandleFunc("POST /rules/{language}/_test", d.testRule)
	mux.HandleFunc("POST /rules/{language}/{id}", d.saveRule)
	mux.HandleFunc("POST /rules/{language}/{id}/delete", d.deleteRule)
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
	ID            int64
	Name          string
	Status        string
	Description   string
	Level         string
	Pattern       string
	Replacement   string
	Before        string
	After         string
	NotBefore     string
	NotAfter      string
	CaseSensitive bool
	Updated       string
	UpdatedBy     string
}

func ruleToUI(r *spell.Rule) uiRule {
	level := "error"
	if r.Level == spell.CorrectionLevel_LEVEL_SUGGESTION {
		level = "suggestion"
	}

	return uiRule{
		ID:            r.Id,
		Name:          r.Name,
		Status:        r.Status,
		Description:   r.Description,
		Level:         level,
		Pattern:       r.Pattern,
		Replacement:   r.Replacement,
		Before:        strings.Join(r.Before, ", "),
		After:         strings.Join(r.After, ", "),
		NotBefore:     strings.Join(r.NotBefore, ", "),
		NotAfter:      strings.Join(r.NotAfter, ", "),
		CaseSensitive: r.CaseSensitive,
		Updated:       r.Updated,
		UpdatedBy:     r.UpdatedBy,
	}
}

type rulesContents struct {
	Languages  []string
	Language   string
	Rules      []uiRule
	Rule       *uiRule
	ActiveRule int64
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
	canWrite := hasWriteScope(ctx)

	id, err := ruleIDParam(r)
	if err != nil {
		return nil, err
	}

	svcCtx, err := bridgeServiceAuth(ctx, d.authParser)
	if err != nil {
		return nil, howdah.InternalHTTPError(err)
	}

	res, err := d.rules.GetRule(svcCtx, &spell.GetRuleRequest{Id: id})
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	rule := ruleToUI(res.Rule)

	if isHtmx(r) {
		return &howdah.Page{
			Template: "rule_form.html",
			Contents: rulesContents{
				Language: lang, Rule: &rule, ActiveRule: id, CanWrite: canWrite,
			},
		}, nil
	}

	rules, hasMore, err := d.listRules(ctx, lang, "", 0)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	return &howdah.Page{
		Template: "rules.html",
		Title:    howdah.TLiteral(rule.Name + " – Rules"),
		Contents: rulesContents{
			Languages:  d.languages,
			Language:   lang,
			Rules:      rules,
			Count:      d.ruleCount(ctx, lang),
			Rule:       &rule,
			ActiveRule: id,
			CanWrite:   canWrite,
			HasMore:    hasMore,
		},
	}, nil
}

func (d *RulesUI) saveNewRule(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	return d.saveRuleForm(ctx, w, r, true)
}

func (d *RulesUI) saveRule(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	return d.saveRuleForm(ctx, w, r, false)
}

// saveRuleForm handles both rule creation (isNew) and updates: they share the
// auth, form parsing and persistence path, differing only in id source, the
// name-required guard, the pushed URL and the flash message.
func (d *RulesUI) saveRuleForm(
	ctx context.Context, w http.ResponseWriter, r *http.Request, isNew bool,
) (*howdah.Page, error) {
	ctx, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	if !hasWriteScope(ctx) {
		return nil, forbiddenScope()
	}

	lang := r.PathValue("language")

	var id int64

	if !isNew {
		id, err = ruleIDParam(r)
		if err != nil {
			return nil, err
		}
	}

	if err := parseForm(r); err != nil {
		return nil, err
	}

	if isNew && strings.TrimSpace(r.FormValue("name")) == "" {
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

	id, err = d.setRuleFromForm(svcCtx, lang, id, r)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	flash := &flashMessage{
		Type:    "success",
		Message: howdah.TL("RuleUpdated", "Rule updated"),
	}

	if isNew {
		w.Header().Set("HX-Push-Url",
			"/rules/"+lang+"/"+strconv.FormatInt(id, 10))

		flash.Message = howdah.TL("RuleCreated", "Rule created")
	}

	return d.ruleDetailResponse(ctx, svcCtx, lang, id, flash)
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

	id, err := ruleIDParam(r)
	if err != nil {
		return nil, err
	}

	svcCtx, err := bridgeServiceAuth(ctx, d.authParser)
	if err != nil {
		return nil, howdah.InternalHTTPError(err)
	}

	_, err = d.rules.DeleteRule(svcCtx, &spell.DeleteRuleRequest{Id: id})
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	w.Header().Set("HX-Push-Url", "/rules/"+lang+"/")

	return d.ruleDetailResponse(ctx, svcCtx, lang, 0, nil)
}

// ruleDetailResponse renders the rule detail (a form, or the empty placeholder
// when id is zero) together with an out-of-band refresh of the sidebar list, so
// the list reflects creates, edits and deletes immediately.
func (d *RulesUI) ruleDetailResponse(
	ctx, svcCtx context.Context, lang string, id int64, flash *flashMessage,
) (*howdah.Page, error) {
	rules, hasMore, err := d.listRules(ctx, lang, "", 0)
	if err != nil {
		return nil, twirpErrorToHTTP(err)
	}

	contents := rulesContents{
		Language: lang,
		Rules:    rules,
		HasMore:  hasMore,
		CanWrite: true,
		Flash:    flash,
	}

	if id != 0 {
		res, err := d.rules.GetRule(svcCtx, &spell.GetRuleRequest{Id: id})
		if err != nil {
			return nil, twirpErrorToHTTP(err)
		}

		rule := ruleToUI(res.Rule)
		contents.Rule = &rule
		contents.ActiveRule = id
	}

	return &howdah.Page{
		Template: "rule_response.html",
		Contents: contents,
	}, nil
}

// setRuleFromForm sets a rule from the submitted form and returns its id. A
// zero id creates a new rule.
func (d *RulesUI) setRuleFromForm(
	ctx context.Context, lang string, id int64, r *http.Request,
) (int64, error) {
	level := spell.CorrectionLevel_LEVEL_ERROR
	if r.FormValue("level") == "suggestion" {
		level = spell.CorrectionLevel_LEVEL_SUGGESTION
	}

	status := strings.TrimSpace(r.FormValue("status"))
	if status == "" {
		status = "accepted"
	}

	res, err := d.rules.SetRule(ctx, &spell.SetRuleRequest{
		Rule: &spell.Rule{
			Id:            id,
			Language:      lang,
			Name:          strings.TrimSpace(r.FormValue("name")),
			Status:        status,
			Description:   strings.TrimSpace(r.FormValue("description")),
			Level:         level,
			Pattern:       strings.TrimSpace(r.FormValue("pattern")),
			Replacement:   strings.TrimSpace(r.FormValue("replacement")),
			Before:        splitCommaList(r.FormValue("before")),
			After:         splitCommaList(r.FormValue("after")),
			NotBefore:     splitCommaList(r.FormValue("not_before")),
			NotAfter:      splitCommaList(r.FormValue("not_after")),
			CaseSensitive: r.FormValue("case_sensitive") == "on",
		},
	})
	if err != nil {
		return 0, fmt.Errorf("set rule: %w", err)
	}

	return res.Id, nil
}

type ruleTestMatch struct {
	Text       string
	Suggestion string
}

type ruleTestContents struct {
	Error   string
	Matches []ruleTestMatch
	Sample  string
}

// testRule compiles the rule being edited and runs it against the sample input,
// reporting the matches and their suggestions. It is read-only — no rule is
// stored.
func (d *RulesUI) testRule(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	_, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	if err := parseForm(r); err != nil {
		return nil, err
	}

	sample := r.FormValue("sample")

	def := RuleDef{
		Pattern:       strings.TrimSpace(r.FormValue("pattern")),
		Replacement:   strings.TrimSpace(r.FormValue("replacement")),
		Before:        splitCommaList(r.FormValue("before")),
		After:         splitCommaList(r.FormValue("after")),
		NotBefore:     splitCommaList(r.FormValue("not_before")),
		NotAfter:      splitCommaList(r.FormValue("not_after")),
		CaseSensitive: r.FormValue("case_sensitive") == "on",
	}

	contents := ruleTestContents{Sample: sample}

	rule, err := compileRule(def)
	if err != nil {
		contents.Error = err.Error()
	} else {
		for _, m := range matchRule(sample, rule) {
			contents.Matches = append(contents.Matches, ruleTestMatch{
				Text:       sample[m.start:m.end],
				Suggestion: m.suggestion,
			})
		}
	}

	return &howdah.Page{
		Template: "rule_test.html",
		Contents: contents,
	}, nil
}

// ruleIDParam reads and parses the {id} path value.
func ruleIDParam(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, howdah.NewHTTPError(
			http.StatusBadRequest, "Error", "Invalid rule id",
			fmt.Errorf("parse rule id: %w", err),
		)
	}

	return id, nil
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
		PageSize: rulesPageSize,
	})
	if err != nil {
		return nil, false, fmt.Errorf("list rules for %q page %d: %w",
			lang, page, err)
	}

	rules := make([]uiRule, len(res.Rules))
	for i, r := range res.Rules {
		rules[i] = ruleToUI(r)
	}

	// A full page implies there may be another; the request fixes the size so
	// the boundary check stays in step with the service.
	return rules, len(res.Rules) == rulesPageSize, nil
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
