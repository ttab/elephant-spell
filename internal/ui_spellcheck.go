package internal

import (
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/ttab/elephant-api/spell"
	"github.com/ttab/elephantine"
	"github.com/ttab/howdah"
)

// SpellcheckUI is an interactive page for testing text against the active
// dictionary and rules — a formatted view of the Check service response.
type SpellcheckUI struct {
	log             *slog.Logger
	auth            howdah.Authenticator
	authParser      elephantine.AuthInfoParser
	check           spell.Check
	languages       []string
	defaultLanguage string
}

func NewSpellcheckUI(
	logger *slog.Logger,
	auth howdah.Authenticator,
	authParser elephantine.AuthInfoParser,
	check spell.Check,
	languages []string,
	defaultLanguage string,
) *SpellcheckUI {
	languages = slices.Clone(languages)
	slices.Sort(languages)

	if defaultLanguage == "" {
		defaultLanguage = languages[0]
	}

	return &SpellcheckUI{
		log:             logger,
		auth:            auth,
		authParser:      authParser,
		check:           check,
		languages:       languages,
		defaultLanguage: defaultLanguage,
	}
}

func (d *SpellcheckUI) GetTemplateFuncs() template.FuncMap {
	return template.FuncMap{}
}

func (d *SpellcheckUI) RegisterRoutes(mux *howdah.PageMux) {
	mux.HandleFunc("GET /spellcheck/{$}", d.page)
	mux.HandleFunc("POST /spellcheck/_check", d.runCheck)
}

func (d *SpellcheckUI) MenuHook(hooks *howdah.MenuHooks) {
	hooks.RegisterHook(func() []howdah.MenuItem {
		return []howdah.MenuItem{
			{
				Title:  howdah.TL("Spellcheck", "Spellcheck"),
				HREF:   "/spellcheck/",
				Weight: 25,
			},
		}
	})
}

type spellSuggestion struct {
	Text        string
	Description string
}

type spellResultEntry struct {
	Text        string
	Level       string
	Status      string
	Spans       string
	Suggestions []spellSuggestion
}

// spellChunk holds the findings for one input chunk (a paragraph/line of the
// submitted text).
type spellChunk struct {
	Text    string
	Entries []spellResultEntry
}

type spellcheckContents struct {
	Languages   []string
	Language    string
	Input       string
	Suggestions bool
	CustomOnly  bool
	Checked     bool
	Chunks      []spellChunk
	TotalIssues int
}

func (d *SpellcheckUI) page(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	_, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	return &howdah.Page{
		Template: "spellcheck.html",
		Title:    howdah.TL("Spellcheck", "Spellcheck"),
		Contents: spellcheckContents{
			Languages:   d.languages,
			Language:    d.preferredLanguage(r),
			Suggestions: true,
		},
	}, nil
}

func (d *SpellcheckUI) runCheck(
	ctx context.Context, w http.ResponseWriter, r *http.Request,
) (*howdah.Page, error) {
	ctx, err := d.auth.RequireAuth(ctx, w, r)
	if err != nil {
		return nil, err
	}

	err = r.ParseForm()
	if err != nil {
		return nil, howdah.NewHTTPError(
			http.StatusBadRequest, "Error", "Invalid form data",
			fmt.Errorf("parse form: %w", err))
	}

	lang := r.FormValue("language")
	if !slices.Contains(d.languages, lang) {
		lang = d.defaultLanguage
	}

	suggestions := r.FormValue("suggestions") == "on"
	customOnly := r.FormValue("custom_only") == "on"

	// Each non-empty line is a chunk, mirroring how clients send paragraphs and
	// headlines as separate strings.
	chunks := splitChunks(r.FormValue("text"))

	contents := spellcheckContents{
		Languages:   d.languages,
		Language:    lang,
		Input:       r.FormValue("text"),
		Suggestions: suggestions,
		CustomOnly:  customOnly,
		Checked:     true,
	}

	if len(chunks) > 0 {
		svcCtx, err := bridgeServiceAuth(ctx, d.authParser)
		if err != nil {
			return nil, howdah.InternalHTTPError(err)
		}

		res, err := d.check.Text(svcCtx, &spell.TextRequest{
			Text:        chunks,
			Language:    lang,
			Suggestions: suggestions,
			CustomOnly:  customOnly,
		})
		if err != nil {
			return nil, twirpErrorToHTTP(err)
		}

		contents.Chunks, contents.TotalIssues = spellChunks(chunks, res)
	}

	return &howdah.Page{
		Template: "spellcheck_result.html",
		Contents: contents,
	}, nil
}

// spellChunks pairs each input chunk with its findings and counts the total.
func spellChunks(
	chunks []string, res *spell.TextResponse,
) ([]spellChunk, int) {
	out := make([]spellChunk, 0, len(chunks))
	total := 0

	for i, text := range chunks {
		var entries []spellResultEntry

		if i < len(res.Misspelled) && res.Misspelled[i] != nil {
			for _, e := range res.Misspelled[i].Entries {
				entries = append(entries, spellResultEntry{
					Text:        e.Text,
					Level:       correctionLevelLabel(e.Level),
					Status:      e.Status,
					Spans:       formatSpans(e.Spans),
					Suggestions: spellSuggestions(e.Suggestions),
				})
			}
		}

		total += len(entries)

		out = append(out, spellChunk{Text: text, Entries: entries})
	}

	return out, total
}

func spellSuggestions(in []*spell.Suggestion) []spellSuggestion {
	out := make([]spellSuggestion, len(in))
	for i, s := range in {
		out[i] = spellSuggestion{Text: s.Text, Description: s.Description}
	}

	return out
}

// formatSpans renders the character ranges as "start–end" pairs for display.
func formatSpans(spans []*spell.TextSpan) string {
	parts := make([]string, len(spans))
	for i, s := range spans {
		parts[i] = fmt.Sprintf("%d–%d", s.Start, s.End)
	}

	return strings.Join(parts, ", ")
}

func correctionLevelLabel(level spell.CorrectionLevel) string {
	if level == spell.CorrectionLevel_LEVEL_SUGGESTION {
		return "suggestion"
	}

	return "error"
}

// preferredLanguage picks the language from the lang cookie when it matches a
// supported language, falling back to the default.
func (d *SpellcheckUI) preferredLanguage(r *http.Request) string {
	if c, err := r.Cookie("lang"); err == nil && c.Value != "" {
		if match := matchLanguage(d.languages, c.Value); match != "" {
			return match
		}
	}

	return d.defaultLanguage
}

// splitChunks breaks the input into one chunk per non-empty line.
func splitChunks(text string) []string {
	var chunks []string

	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			chunks = append(chunks, line)
		}
	}

	return chunks
}
