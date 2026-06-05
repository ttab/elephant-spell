package internal

import (
	"bytes"
	"html/template"
	"net/url"
	"strings"
	"testing"
)

// renderEntryForm parses and executes the entry form template in isolation,
// with stub localisation funcs, so template execution errors (not just parse
// errors) are caught in unit tests.
func renderEntryForm(t *testing.T, contents dictionariesContents) string {
	t.Helper()

	funcs := template.FuncMap{
		"t": func(args ...string) string {
			if len(args) > 0 {
				return args[len(args)-1]
			}

			return ""
		},
		"tl":            func(_ ...any) string { return "" },
		"td":            func(_ ...any) string { return "" },
		"pathEscape":    url.PathEscape,
		"expandPreview": mistakesPreview,
	}

	tpl, err := template.New("entry_form.html").Funcs(funcs).
		ParseFiles("../templates/entry_form.html", "../templates/pattern_preview.html")
	if err != nil {
		t.Fatalf("parse template: %v", err)
	}

	var buf bytes.Buffer

	err = tpl.ExecuteTemplate(&buf, "entry_form.html", struct {
		Contents dictionariesContents
	}{Contents: contents})
	if err != nil {
		t.Fatalf("execute template: %v", err)
	}

	return buf.String()
}

func renderRuleForm(t *testing.T, contents rulesContents) string {
	t.Helper()

	funcs := template.FuncMap{
		"t": func(args ...string) string {
			if len(args) > 0 {
				return args[len(args)-1]
			}

			return ""
		},
		"tl":         func(_ ...any) string { return "" },
		"td":         func(_ ...any) string { return "" },
		"pathEscape": url.PathEscape,
	}

	tpl, err := template.New("rule_form.html").Funcs(funcs).
		ParseFiles("../templates/rule_form.html")
	if err != nil {
		t.Fatalf("parse template: %v", err)
	}

	var buf bytes.Buffer

	err = tpl.ExecuteTemplate(&buf, "rule_form.html", struct {
		Contents rulesContents
	}{Contents: contents})
	if err != nil {
		t.Fatalf("execute template: %v", err)
	}

	return buf.String()
}

func renderSpellcheckResult(t *testing.T, contents spellcheckContents) string {
	t.Helper()

	funcs := template.FuncMap{
		"t": func(args ...string) string {
			if len(args) > 0 {
				return args[len(args)-1]
			}

			return ""
		},
		"tl": func(_ ...any) string { return "" },
		"td": func(_ ...any) string { return "" },
	}

	tpl, err := template.New("spellcheck_result.html").Funcs(funcs).
		ParseFiles("../templates/spellcheck_result.html")
	if err != nil {
		t.Fatalf("parse template: %v", err)
	}

	var buf bytes.Buffer

	err = tpl.ExecuteTemplate(&buf, "spellcheck_result.html", struct {
		Contents spellcheckContents
	}{Contents: contents})
	if err != nil {
		t.Fatalf("execute template: %v", err)
	}

	return buf.String()
}

func TestSpellcheckResultRender(t *testing.T) {
	t.Run("no issues", func(t *testing.T) {
		out := renderSpellcheckResult(t, spellcheckContents{
			Checked: true, TotalIssues: 0,
		})

		if !strings.Contains(out, "No issues found") {
			t.Errorf("expected no-issues message, got %q", out)
		}
	})

	t.Run("with findings", func(t *testing.T) {
		out := renderSpellcheckResult(t, spellcheckContents{
			Checked:     true,
			TotalIssues: 1,
			Chunks: []spellChunk{
				{
					Text: "Mexico City",
					Entries: []spellResultEntry{
						{
							Text:   "Mexico",
							Level:  "error",
							Status: "accepted",
							Suggestions: []spellSuggestion{
								{Text: "Mexiko", Description: "spelling"},
							},
						},
					},
				},
			},
		})

		for _, want := range []string{"Mexico", "Mexiko", "accepted", "spelling"} {
			if !strings.Contains(out, want) {
				t.Errorf("result missing %q in %q", want, out)
			}
		}
	})

	t.Run("not checked renders empty", func(t *testing.T) {
		out := renderSpellcheckResult(t, spellcheckContents{})

		if strings.TrimSpace(out) != "" {
			t.Errorf("expected empty output before a check, got %q", out)
		}
	})
}

func TestRuleFormRender(t *testing.T) {
	t.Run("new rule", func(t *testing.T) {
		out := renderRuleForm(t, rulesContents{
			Language: "sv-se",
			NewRule:  true,
			CanWrite: true,
		})

		for _, want := range []string{
			`name="pattern"`, `name="replacement"`, `name="not_before"`,
			`name="sample"`, "/sv-se/_test",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("new-rule form missing %q", want)
			}
		}
	})

	t.Run("existing rule", func(t *testing.T) {
		out := renderRuleForm(t, rulesContents{
			Language: "sv-se",
			CanWrite: true,
			Rule: &uiRule{
				Name:        "dash",
				Status:      "accepted",
				Level:       "error",
				Pattern:     "{digit}-{digit}",
				Replacement: "{1}–{2}",
				NotBefore:   "att",
			},
		})

		for _, want := range []string{
			// pattern/replacement render as code-input element content
			`>{digit}-{digit}</code-input>`,
			`name="not_before" value="att"`,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("rule form missing %q", want)
			}
		}
	})
}

func TestEntryFormRender(t *testing.T) {
	t.Run("new entry", func(t *testing.T) {
		out := renderEntryForm(t, dictionariesContents{
			Language: "sv-se",
			NewEntry: true,
			CanWrite: true,
		})

		for _, want := range []string{
			`template="spell-pattern"`,
			"data-add-form-row",
			`id="forms-row-template"`,
			"common-mistakes-preview",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("new-entry form missing %q", want)
			}
		}
	})

	t.Run("existing entry with forms and mistakes", func(t *testing.T) {
		out := renderEntryForm(t, dictionariesContents{
			Language: "sv-se",
			CanWrite: true,
			Entry: &uiEntry{
				Entry:          "fängelse",
				Status:         "accepted",
				Level:          "error",
				CommonMistakes: []string{"{a|b} c"},
				Forms:          map[string]string{"kriminalvårdsanstalten": "fängelset"},
			},
		})

		for _, want := range []string{
			`name="forms_incorrect" value="kriminalvårdsanstalten"`,
			`name="forms_correct" value="fängelset"`,
			"{a|b} c",
			// the expansion preview is computed at load, without editing
			"Total expansions",
			"pattern-preview-list",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("entry form missing %q", want)
			}
		}
	})

	t.Run("literal-only mistakes show no preview", func(t *testing.T) {
		out := renderEntryForm(t, dictionariesContents{
			Language: "sv-se",
			CanWrite: true,
			Entry: &uiEntry{
				Entry:          "fängelse",
				Level:          "error",
				CommonMistakes: []string{"kriminalvårdsanstalt", "fängelsre"},
			},
		})

		if strings.Contains(out, "pattern-preview-list") ||
			strings.Contains(out, "Total expansions") {
			t.Error("literal-only common mistakes should not render a preview")
		}
	})

	t.Run("read-only hides editing controls", func(t *testing.T) {
		out := renderEntryForm(t, dictionariesContents{
			Language: "sv-se",
			CanWrite: false,
			Entry: &uiEntry{
				Entry: "fängelse",
				Level: "error",
				Forms: map[string]string{"x": "y"},
			},
		})

		if strings.Contains(out, "data-add-form-row") {
			t.Error("read-only form should not offer an add-row button")
		}

		if !strings.Contains(out, "readonly") {
			t.Error("read-only form should mark the pattern editor readonly")
		}
	})
}
