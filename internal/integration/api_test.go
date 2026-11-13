package integration_test

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	connect "connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	spellapi "github.com/ttab/elephant-api/spell"
	"github.com/ttab/elephant-spell/internal/integration"
	"github.com/twitchtv/twirp"
)

// TestAPI drives the full spell API surface over the wire against a real
// server started via Application.Run, with real Elsinod-issued JWTs validated
// by the production auth middleware.
func TestAPI(t *testing.T) {
	stack := integration.NewStack(t)
	ctx := t.Context()

	t.Run("supported_languages", func(t *testing.T) {
		res, err := stack.Dictionaries.SupportedLanguages(ctx,
			&spellapi.SupportedLanguagesRequest{})
		if err != nil {
			t.Fatalf("supported languages: %v", err)
		}

		var codes []string
		for _, l := range res.Languages {
			codes = append(codes, l.Code)
		}

		if !slices.Contains(codes, "sv-se") {
			t.Fatalf("expected sv-se among supported languages, got %v", codes)
		}
	})

	t.Run("set_get_list_entry", func(t *testing.T) {
		const text = "Liechtenstein"

		_, err := stack.Dictionaries.SetEntry(ctx, &spellapi.SetEntryRequest{
			Entry: &spellapi.CustomEntry{
				Language:      "sv-se",
				Text:          text,
				Status:        "approved",
				Description:   "a small country",
				Level:         spellapi.CorrectionLevel_LEVEL_ERROR,
				CaseSensitive: true,
			},
		})
		if err != nil {
			t.Fatalf("set entry: %v", err)
		}

		got, err := stack.Dictionaries.GetEntry(ctx, &spellapi.GetEntryRequest{
			Language: "sv-se",
			Text:     text,
		})
		if err != nil {
			t.Fatalf("get entry: %v", err)
		}

		if got.Entry == nil || got.Entry.Text != text {
			t.Fatalf("unexpected entry: %+v", got.Entry)
		}

		if !got.Entry.CaseSensitive {
			t.Errorf("CaseSensitive did not round-trip")
		}

		if got.Entry.UpdatedBy != stack.Admin.Subject {
			t.Errorf("UpdatedBy = %q, want %q",
				got.Entry.UpdatedBy, stack.Admin.Subject)
		}

		list, err := stack.Dictionaries.ListEntries(ctx,
			&spellapi.ListEntriesRequest{Language: "sv-se", Query: "Liech"})
		if err != nil {
			t.Fatalf("list entries: %v", err)
		}

		if !slices.ContainsFunc(list.Entries, func(e *spellapi.CustomEntry) bool {
			return e.Text == text
		}) {
			t.Fatalf("listed entries missing %q: %+v", text, list.Entries)
		}
	})

	t.Run("rename_entry", func(t *testing.T) {
		_, err := stack.Dictionaries.SetEntry(ctx, &spellapi.SetEntryRequest{
			Entry: &spellapi.CustomEntry{
				Language:       "sv-se",
				Text:           "Faroe",
				Status:         "accepted",
				Level:          spellapi.CorrectionLevel_LEVEL_ERROR,
				CommonMistakes: []string{"Färö"},
				CaseSensitive:  true,
			},
		})
		if err != nil {
			t.Fatalf("set entry: %v", err)
		}

		_, err = stack.Dictionaries.SetEntry(ctx, &spellapi.SetEntryRequest{
			Entry: &spellapi.CustomEntry{
				Language: "sv-se", Text: "Iceland", Status: "accepted",
			},
		})
		if err != nil {
			t.Fatalf("set conflicting entry: %v", err)
		}

		_, err = stack.Dictionaries.RenameEntry(ctx, &spellapi.RenameEntryRequest{
			Language: "sv-se", Text: "Faroe", NewText: "Faroes",
		})
		if err != nil {
			t.Fatalf("rename: %v", err)
		}

		// The renamed entry exists and kept its data.
		got, err := stack.Dictionaries.GetEntry(ctx, &spellapi.GetEntryRequest{
			Language: "sv-se", Text: "Faroes",
		})
		if err != nil {
			t.Fatalf("get renamed: %v", err)
		}

		if !got.Entry.CaseSensitive || len(got.Entry.CommonMistakes) != 1 {
			t.Errorf("rename did not preserve data: %+v", got.Entry)
		}

		// The old text is gone.
		_, err = stack.Dictionaries.GetEntry(ctx, &spellapi.GetEntryRequest{
			Language: "sv-se", Text: "Faroe",
		})
		if err == nil {
			t.Error("old entry text should no longer exist")
		}

		// Renaming onto an existing text conflicts.
		_, err = stack.Dictionaries.RenameEntry(ctx, &spellapi.RenameEntryRequest{
			Language: "sv-se", Text: "Faroes", NewText: "Iceland",
		})
		assertCode(t, err, connect.CodeAlreadyExists)

		// Renaming a missing entry is a not-found.
		_, err = stack.Dictionaries.RenameEntry(ctx, &spellapi.RenameEntryRequest{
			Language: "sv-se", Text: "Nonexistent", NewText: "Whatever",
		})
		assertCode(t, err, connect.CodeNotFound)
	})

	t.Run("spellcheck_flow_through_eventlog", func(t *testing.T) {
		const (
			entry   = "Gaddafi"
			mistake = "Gadaffi"
		)

		flagged := func() bool {
			res, err := stack.Check.Text(ctx, &spellapi.TextRequest{
				Language:   "sv-se",
				Text:       []string{mistake},
				CustomOnly: true,
			})
			if err != nil {
				t.Fatalf("spellcheck: %v", err)
			}

			return len(res.Misspelled) == 1 && len(res.Misspelled[0].Entries) > 0
		}

		if flagged() {
			t.Fatal("precondition: mistake should not be flagged yet")
		}

		_, err := stack.Dictionaries.SetEntry(ctx, &spellapi.SetEntryRequest{
			Entry: &spellapi.CustomEntry{
				Language:       "sv-se",
				Text:           entry,
				Status:         "approved",
				CommonMistakes: []string{mistake},
				Level:          spellapi.CorrectionLevel_LEVEL_ERROR,
			},
		})
		if err != nil {
			t.Fatalf("set entry: %v", err)
		}

		// The change reaches the spellchecker asynchronously via the eventlog
		// (RPC commit → NOTIFY → subscriber → FanOut → drain).
		integration.WaitFor(t, "entry to propagate", flagged)

		_, err = stack.Dictionaries.DeleteEntry(ctx, &spellapi.DeleteEntryRequest{
			Language: "sv-se",
			Text:     entry,
		})
		if err != nil {
			t.Fatalf("delete entry: %v", err)
		}

		integration.WaitFor(t, "delete to propagate", func() bool {
			return !flagged()
		})
	})

	t.Run("suggestion_level_round_trip", func(t *testing.T) {
		const text = "Göteborg"

		_, err := stack.Dictionaries.SetEntry(ctx, &spellapi.SetEntryRequest{
			Entry: &spellapi.CustomEntry{
				Language: "sv-se",
				Text:     text,
				Status:   "approved",
				Level:    spellapi.CorrectionLevel_LEVEL_SUGGESTION,
			},
		})
		if err != nil {
			t.Fatalf("set entry: %v", err)
		}

		got, err := stack.Dictionaries.GetEntry(ctx, &spellapi.GetEntryRequest{
			Language: "sv-se",
			Text:     text,
		})
		if err != nil {
			t.Fatalf("get entry: %v", err)
		}

		if got.Entry.Level != spellapi.CorrectionLevel_LEVEL_SUGGESTION {
			t.Fatalf("level round-trip: got %v", got.Entry.Level)
		}
	})

	t.Run("input_validation", func(t *testing.T) {
		cases := []struct {
			name string
			call func() error
		}{
			{"set entry nil", func() error {
				_, err := stack.Dictionaries.SetEntry(ctx,
					&spellapi.SetEntryRequest{})

				return err
			}},
			{"set unknown language", func() error {
				_, err := stack.Dictionaries.SetEntry(ctx, &spellapi.SetEntryRequest{
					Entry: &spellapi.CustomEntry{
						Language: "zz-zz", Text: "x", Status: "approved",
					},
				})

				return err
			}},
			{"set missing text", func() error {
				_, err := stack.Dictionaries.SetEntry(ctx, &spellapi.SetEntryRequest{
					Entry: &spellapi.CustomEntry{Language: "sv-se", Status: "approved"},
				})

				return err
			}},
			{"set missing status", func() error {
				_, err := stack.Dictionaries.SetEntry(ctx, &spellapi.SetEntryRequest{
					Entry: &spellapi.CustomEntry{Language: "sv-se", Text: "x"},
				})

				return err
			}},
			{"set invalid level", func() error {
				_, err := stack.Dictionaries.SetEntry(ctx, &spellapi.SetEntryRequest{
					Entry: &spellapi.CustomEntry{
						Language: "sv-se", Text: "x", Status: "approved",
						Level: spellapi.CorrectionLevel(99),
					},
				})

				return err
			}},
			{"delete missing language", func() error {
				_, err := stack.Dictionaries.DeleteEntry(ctx,
					&spellapi.DeleteEntryRequest{Text: "x"})

				return err
			}},
			{"get missing text", func() error {
				_, err := stack.Dictionaries.GetEntry(ctx,
					&spellapi.GetEntryRequest{Language: "sv-se"})

				return err
			}},
			{"list entries bad query", func() error {
				_, err := stack.Dictionaries.ListEntries(ctx,
					&spellapi.ListEntriesRequest{Query: "a%b"})

				return err
			}},
			{"check unsupported language", func() error {
				_, err := stack.Check.Text(ctx, &spellapi.TextRequest{
					Language: "zz-zz", Text: []string{"hej"},
				})

				return err
			}},
			{"suggestions missing text", func() error {
				_, err := stack.Check.Suggestions(ctx,
					&spellapi.SuggestionsRequest{Language: "sv-se"})

				return err
			}},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				assertCode(t, tc.call(), connect.CodeInvalidArgument)
			})
		}
	})

	t.Run("suggestions", func(t *testing.T) {
		// Just exercise the handler end to end; the suggestion set itself
		// depends on the bundled hunspell dictionary.
		_, err := stack.Check.Suggestions(ctx, &spellapi.SuggestionsRequest{
			Language: "sv-se",
			Text:     "hjälpp",
		})
		if err != nil {
			t.Fatalf("suggestions: %v", err)
		}
	})

	t.Run("list_dictionaries", func(t *testing.T) {
		res, err := stack.Dictionaries.ListDictionaries(ctx,
			&spellapi.ListDictionariesRequest{})
		if err != nil {
			t.Fatalf("list dictionaries: %v", err)
		}

		if !slices.ContainsFunc(res.Dictionaries,
			func(d *spellapi.CustomDictionary) bool {
				return d.Language == "sv-se" && d.EntryCount > 0
			}) {
			t.Fatalf("expected a non-empty sv-se dictionary, got %+v",
				res.Dictionaries)
		}
	})

	t.Run("unauthenticated_is_rejected", func(t *testing.T) {
		anon := spellapi.NewCheckProtobufClient(stack.BaseURL, http.DefaultClient)

		_, err := anon.Text(ctx, &spellapi.TextRequest{
			Language: "sv-se",
			Text:     []string{"hej"},
		})

		assertCode(t, err, connect.CodeUnauthenticated)
	})

	t.Run("write_requires_scope", func(t *testing.T) {
		// A caller authenticated but lacking spell_write.
		reader := stack.Env.Caller(t, "reader", "openid")

		_, err := stack.DictionariesClient(reader).SetEntry(ctx,
			&spellapi.SetEntryRequest{
				Entry: &spellapi.CustomEntry{
					Language: "sv-se",
					Text:     "Borås",
					Status:   "approved",
					Level:    spellapi.CorrectionLevel_LEVEL_ERROR,
				},
			})

		assertCode(t, err, connect.CodePermissionDenied)
	})

	t.Run("moderation_status_flow", func(t *testing.T) {
		const (
			entry   = "Pyongyang"
			mistake = "Pjongjang"
		)

		_, err := stack.Dictionaries.SetEntry(ctx, &spellapi.SetEntryRequest{
			Entry: &spellapi.CustomEntry{
				Language:       "sv-se",
				Text:           entry,
				Status:         "pending",
				CommonMistakes: []string{mistake},
				Level:          spellapi.CorrectionLevel_LEVEL_ERROR,
			},
		})
		if err != nil {
			t.Fatalf("set pending entry: %v", err)
		}

		// statusFor returns the status the spellchecker reports for the
		// mistake, or "" if it isn't flagged yet.
		statusFor := func() string {
			res, err := stack.Check.Text(ctx, &spellapi.TextRequest{
				Language:   "sv-se",
				Text:       []string{mistake},
				CustomOnly: true,
			})
			if err != nil {
				t.Fatalf("spellcheck: %v", err)
			}

			if len(res.Misspelled) == 1 && len(res.Misspelled[0].Entries) > 0 {
				return res.Misspelled[0].Entries[0].Status
			}

			return ""
		}

		integration.WaitFor(t, "pending entry to propagate", func() bool {
			return statusFor() == "pending"
		})

		// The pending entry should show up in the per-language pending count.
		dicts, err := stack.Dictionaries.ListDictionaries(ctx,
			&spellapi.ListDictionariesRequest{})
		if err != nil {
			t.Fatalf("list dictionaries: %v", err)
		}

		if !slices.ContainsFunc(dicts.Dictionaries,
			func(d *spellapi.CustomDictionary) bool {
				return d.Language == "sv-se" && d.PendingCount > 0
			}) {
			t.Fatalf("expected a pending entry for sv-se, got %+v",
				dicts.Dictionaries)
		}

		// Filtering by status returns only pending entries, and page_size
		// bounds the page.
		pending, err := stack.Dictionaries.ListEntries(ctx,
			&spellapi.ListEntriesRequest{
				Language: "sv-se", Status: "pending", PageSize: 1,
			})
		if err != nil {
			t.Fatalf("list pending entries: %v", err)
		}

		if len(pending.Entries) != 1 {
			t.Fatalf("page_size=1 should bound to one entry, got %d",
				len(pending.Entries))
		}

		// Accepting flips the status, and it propagates back to the checker.
		_, err = stack.Dictionaries.SetEntryStatus(ctx,
			&spellapi.SetEntryStatusRequest{
				Language: "sv-se", Text: entry, Status: "accepted",
			})
		if err != nil {
			t.Fatalf("set entry status: %v", err)
		}

		integration.WaitFor(t, "accepted status to propagate", func() bool {
			return statusFor() == "accepted"
		})
	})

	t.Run("set_entry_status_missing", func(t *testing.T) {
		_, err := stack.Dictionaries.SetEntryStatus(ctx,
			&spellapi.SetEntryStatusRequest{
				Language: "sv-se", Text: "does-not-exist", Status: "accepted",
			})

		assertCode(t, err, connect.CodeNotFound)
	})

	t.Run("search_matches_mistakes_and_description", func(t *testing.T) {
		const text = "Zürich"

		_, err := stack.Dictionaries.SetEntry(ctx, &spellapi.SetEntryRequest{
			Entry: &spellapi.CustomEntry{
				Language:       "sv-se",
				Text:           text,
				Status:         "accepted",
				Description:    "kanton i Schweiz",
				CommonMistakes: []string{"Zurich"},
				Level:          spellapi.CorrectionLevel_LEVEL_ERROR,
			},
		})
		if err != nil {
			t.Fatalf("set entry: %v", err)
		}

		finds := func(query string) bool {
			list, err := stack.Dictionaries.ListEntries(ctx,
				&spellapi.ListEntriesRequest{Language: "sv-se", Query: query})
			if err != nil {
				t.Fatalf("list entries: %v", err)
			}

			return slices.ContainsFunc(list.Entries,
				func(e *spellapi.CustomEntry) bool {
					return e.Text == text
				})
		}

		// Substring of the common mistake, the description, and the entry text.
		for _, q := range []string{"Zurich", "kanton", "ürich"} {
			if !finds(q) {
				t.Errorf("query %q did not find %q", q, text)
			}
		}

		if finds("nonexistent-substring") {
			t.Errorf("unrelated query unexpectedly matched %q", text)
		}
	})

	t.Run("custom_rule_flow", func(t *testing.T) {
		created, err := stack.Rules.SetRule(ctx, &spellapi.SetRuleRequest{
			Rule: &spellapi.Rule{
				Language:    "sv-se",
				Name:        "kronor-rule",
				Status:      "accepted",
				Level:       spellapi.CorrectionLevel_LEVEL_SUGGESTION,
				Pattern:     "{digit} kr",
				Replacement: "{1} kronor",
			},
		})
		if err != nil {
			t.Fatalf("set rule: %v", err)
		}

		if created.Id == 0 {
			t.Fatal("SetRule did not return a new id")
		}

		flagged := func() bool {
			res, err := stack.Check.Text(ctx, &spellapi.TextRequest{
				Language:   "sv-se",
				Text:       []string{"Det kostar 5 kr."},
				CustomOnly: true,
			})
			if err != nil {
				t.Fatalf("check: %v", err)
			}

			return len(res.Misspelled) == 1 &&
				slices.ContainsFunc(res.Misspelled[0].Entries,
					func(e *spellapi.MisspelledEntry) bool {
						return e.Text == "5 kr"
					})
		}

		integration.WaitFor(t, "rule to propagate", flagged)

		sug, err := stack.Check.Suggestions(ctx, &spellapi.SuggestionsRequest{
			Language:   "sv-se",
			Text:       "5 kr",
			CustomOnly: true,
		})
		if err != nil {
			t.Fatalf("suggestions: %v", err)
		}

		if !slices.ContainsFunc(sug.Suggestions,
			func(s *spellapi.Suggestion) bool {
				return s.Text == "5 kronor"
			}) {
			t.Errorf("expected '5 kronor' suggestion, got %+v", sug.Suggestions)
		}

		got, err := stack.Rules.GetRule(ctx, &spellapi.GetRuleRequest{
			Id: created.Id,
		})
		if err != nil {
			t.Fatalf("get rule: %v", err)
		}

		if got.Rule == nil || got.Rule.Pattern != "{digit} kr" ||
			got.Rule.Name != "kronor-rule" {
			t.Errorf("rule did not round-trip: %+v", got.Rule)
		}

		// The rule shows up in the rule listing, filtered by pattern substring.
		list, err := stack.Rules.ListRules(ctx, &spellapi.ListRulesRequest{
			Language: "sv-se", Query: "kr",
		})
		if err != nil {
			t.Fatalf("list rules: %v", err)
		}

		if !slices.ContainsFunc(list.Rules, func(r *spellapi.Rule) bool {
			return r.Name == "kronor-rule"
		}) {
			t.Errorf("rule missing from listing: %+v", list.Rules)
		}
	})

	t.Run("phrase_word_limit", func(t *testing.T) {
		const sixWords = "ett två tre fyra fem sex"

		set := func(e *spellapi.CustomEntry) error {
			e.Language = "sv-se"
			e.Status = "accepted"

			_, err := stack.Dictionaries.SetEntry(ctx,
				&spellapi.SetEntryRequest{Entry: e})

			return err
		}

		err := set(&spellapi.CustomEntry{
			Text:           "ett två tre fyra fem",
			CommonMistakes: []string{"ett två tre fyra fyra"},
		})
		if err != nil {
			t.Fatalf("set five-word entry: %v", err)
		}

		cases := []struct {
			name  string
			field string
			call  func() error
		}{
			{"text", "entry.text", func() error {
				return set(&spellapi.CustomEntry{Text: sixWords})
			}},
			{"common mistake after expansion", "entry.common_mistakes", func() error {
				return set(&spellapi.CustomEntry{
					Text:           "ett",
					CommonMistakes: []string{"{ettan|en två} tre fyra fem sex"},
				})
			}},
			{"form", "entry.forms", func() error {
				return set(&spellapi.CustomEntry{
					Text:  "ett",
					Forms: map[string]string{sixWords: "ett"},
				})
			}},
			{"common mistakes over the expansion limit", "entry.common_mistakes", func() error {
				// 3 × 67 = 201 strings, one more than the default limit.
				options := make([]string, 67)
				for i := range options {
					options[i] = fmt.Sprintf("x%d", i)
				}

				return set(&spellapi.CustomEntry{
					Text: "ett",
					CommonMistakes: []string{
						"{a|b|c} {" + strings.Join(options, "|") + "}",
					},
				})
			}},
			{"pathological pattern", "entry.common_mistakes", func() error {
				return set(&spellapi.CustomEntry{
					Text:           "ett",
					CommonMistakes: []string{strings.Repeat("{a|b}", 100)},
				})
			}},
			{"rename", "new_text", func() error {
				_, err := stack.Dictionaries.RenameEntry(ctx,
					&spellapi.RenameEntryRequest{
						Language: "sv-se",
						Text:     "ett två tre fyra fem",
						NewText:  sixWords,
					})

				return err
			}},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				err := tc.call()

				assertCode(t, err, connect.CodeInvalidArgument)

				if !strings.Contains(err.Error(), tc.field) {
					t.Errorf("error does not name %s: %v", tc.field, err)
				}
			})
		}

		t.Run("expansion limit named", func(t *testing.T) {
			err := set(&spellapi.CustomEntry{
				Text:           "ett",
				CommonMistakes: []string{strings.Repeat("{a|b}", 8)},
			})

			assertCode(t, err, connect.CodeInvalidArgument)

			if !strings.Contains(err.Error(), "256 strings, more than the 200") {
				t.Errorf("error does not name the count and limit: %v", err)
			}
		})
	})

	t.Run("invalid_rule_rejected", func(t *testing.T) {
		_, err := stack.Rules.SetRule(ctx, &spellapi.SetRuleRequest{
			Rule: &spellapi.Rule{
				Language: "sv-se",
				Name:     "bad-rule",
				Status:   "accepted",
				Pattern:  "{gap(x)}",
			},
		})

		assertCode(t, err, connect.CodeInvalidArgument)
	})

	t.Run("rule_status_and_delete", func(t *testing.T) {
		created, err := stack.Rules.SetRule(ctx, &spellapi.SetRuleRequest{
			Rule: &spellapi.Rule{
				Language: "sv-se", Name: "pending-rule", Status: "pending",
				Pattern: "{digit} %", Replacement: "{1} procent",
			},
		})
		if err != nil {
			t.Fatalf("set pending rule: %v", err)
		}

		// Updating by id keeps the same rule.
		updated, err := stack.Rules.SetRule(ctx, &spellapi.SetRuleRequest{
			Rule: &spellapi.Rule{
				Id: created.Id, Language: "sv-se", Name: "renamed-rule",
				Status: "pending", Pattern: "{digit} %", Replacement: "{1} procent",
			},
		})
		if err != nil {
			t.Fatalf("update rule: %v", err)
		}

		if updated.Id != created.Id {
			t.Fatalf("update changed the id: %d -> %d", created.Id, updated.Id)
		}

		_, err = stack.Rules.SetRuleStatus(ctx, &spellapi.SetRuleStatusRequest{
			Id: created.Id, Status: "accepted",
		})
		if err != nil {
			t.Fatalf("set rule status: %v", err)
		}

		_, err = stack.Rules.DeleteRule(ctx, &spellapi.DeleteRuleRequest{
			Id: created.Id,
		})
		if err != nil {
			t.Fatalf("delete rule: %v", err)
		}

		_, err = stack.Rules.SetRuleStatus(ctx, &spellapi.SetRuleStatusRequest{
			Id: 999999, Status: "accepted",
		})
		assertCode(t, err, connect.CodeNotFound)
	})

	t.Run("rule_update_keeps_its_language", func(t *testing.T) {
		rule := &spellapi.Rule{
			Language: "sv-se", Name: "mm-rule", Status: "accepted",
			Pattern: "{digit} mm", Replacement: "{1} millimeter",
		}

		created, err := stack.Rules.SetRule(ctx,
			&spellapi.SetRuleRequest{Rule: rule})
		if err != nil {
			t.Fatalf("set rule: %v", err)
		}

		// An update that names another language is refused and changes
		// nothing.
		_, err = stack.Rules.SetRule(ctx, &spellapi.SetRuleRequest{
			Rule: &spellapi.Rule{
				Id: created.Id, Language: "en-gb", Name: "moved-rule",
				Status: "accepted", Pattern: "{digit} mm",
				Replacement: "{1} millimetre",
			},
		})
		assertCode(t, err, connect.CodeInvalidArgument)

		got, err := stack.Rules.GetRule(ctx,
			&spellapi.GetRuleRequest{Id: created.Id})
		if err != nil {
			t.Fatalf("get rule: %v", err)
		}

		if got.Rule.Language != "sv-se" || got.Rule.Name != "mm-rule" {
			t.Errorf("refused update changed the rule: %+v", got.Rule)
		}

		// An update of a rule that does not exist is still not found.
		_, err = stack.Rules.SetRule(ctx, &spellapi.SetRuleRequest{
			Rule: &spellapi.Rule{
				Id: 999999, Language: "sv-se", Name: "missing",
				Status: "accepted", Pattern: "{digit} mm",
			},
		})
		assertCode(t, err, connect.CodeNotFound)
	})

	t.Run("get_entry_missing", func(t *testing.T) {
		_, err := stack.Dictionaries.GetEntry(ctx, &spellapi.GetEntryRequest{
			Language: "sv-se", Text: "does-not-exist",
		})
		assertCode(t, err, connect.CodeNotFound)
	})

	t.Run("negative_page", func(t *testing.T) {
		_, err := stack.Dictionaries.ListEntries(ctx,
			&spellapi.ListEntriesRequest{Language: "sv-se", Page: -1})
		assertCode(t, err, connect.CodeInvalidArgument)

		_, err = stack.Rules.ListRules(ctx,
			&spellapi.ListRulesRequest{Language: "sv-se", Page: -1})
		assertCode(t, err, connect.CodeInvalidArgument)

		// A page size over the maximum is capped, not refused.
		_, err = stack.Dictionaries.ListEntries(ctx,
			&spellapi.ListEntriesRequest{Language: "sv-se", PageSize: 100000})
		if err != nil {
			t.Errorf("list entries with a large page size: %v", err)
		}
	})

	t.Run("rename_onto_concurrent_create", func(t *testing.T) {
		const (
			source = "Racesource"
			target = "Racetarget"
		)

		_, err := stack.Dictionaries.SetEntry(ctx, &spellapi.SetEntryRequest{
			Entry: &spellapi.CustomEntry{
				Language: "sv-se", Text: source, Status: "accepted",
			},
		})
		if err != nil {
			t.Fatalf("set entry: %v", err)
		}

		// Another writer creates the target and has not committed yet, so
		// the rename finds no target and then waits on the key.
		tx, err := stack.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin transaction: %v", err)
		}

		defer func() {
			// Only rolls back when the test stopped before the commit.
			err := tx.Rollback(ctx)
			if err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				t.Errorf("roll back the target: %v", err)
			}
		}()

		_, err = tx.Exec(ctx, `
			INSERT INTO entry(language, entry, status, description,
			       common_mistakes, level, updated_by)
			VALUES ('sv-se', $1, 'accepted', '', '{}', 'error', 'test')`,
			target)
		if err != nil {
			t.Fatalf("create the target: %v", err)
		}

		renamed := make(chan error, 1)

		go func() {
			_, err := stack.Dictionaries.RenameEntry(ctx,
				&spellapi.RenameEntryRequest{
					Language: "sv-se", Text: source, NewText: target,
				})

			renamed <- err
		}()

		integration.WaitFor(t, "the rename to wait for the target's key",
			func() bool {
				var waiting int

				err := stack.Pool.QueryRow(ctx, `
					SELECT COUNT(*) FROM pg_stat_activity
					WHERE wait_event_type = 'Lock'
					      AND query LIKE '%UPDATE entry%'`,
				).Scan(&waiting)
				if err != nil {
					t.Fatalf("read pg_stat_activity: %v", err)
				}

				return waiting > 0
			})

		err = tx.Commit(ctx)
		if err != nil {
			t.Fatalf("commit the target: %v", err)
		}

		assertCode(t, <-renamed, connect.CodeAlreadyExists)
	})

	t.Run("language_codes_resolve_like_checks", func(t *testing.T) {
		const text = "Kasusland"

		_, err := stack.Dictionaries.SetEntry(ctx, &spellapi.SetEntryRequest{
			Entry: &spellapi.CustomEntry{
				Language:       "SV-SE",
				Text:           text,
				Status:         "accepted",
				CommonMistakes: []string{"Kasuslant"},
			},
		})
		if err != nil {
			t.Fatalf("set entry with an upper-case language: %v", err)
		}

		for _, lang := range []string{"sv-se", "SV-SE"} {
			got, err := stack.Dictionaries.GetEntry(ctx,
				&spellapi.GetEntryRequest{Language: lang, Text: text})
			if err != nil {
				t.Fatalf("get entry as %s: %v", lang, err)
			}

			if got.Entry.Language != "sv-se" {
				t.Errorf("stored language %q, want sv-se",
					got.Entry.Language)
			}
		}

		// The entry reaches the spellchecker that Text resolves "SV-SE" to.
		integration.WaitFor(t, "entry to propagate", func() bool {
			res, err := stack.Check.Text(ctx, &spellapi.TextRequest{
				Language:   "SV-SE",
				Text:       []string{"Kasuslant"},
				CustomOnly: true,
			})
			if err != nil {
				t.Fatalf("spellcheck: %v", err)
			}

			return len(res.Misspelled) == 1 &&
				len(res.Misspelled[0].Entries) > 0
		})

		rule, err := stack.Rules.SetRule(ctx, &spellapi.SetRuleRequest{
			Rule: &spellapi.Rule{
				Language: "SV-SE", Name: "cm-rule", Status: "accepted",
				Pattern: "{digit} cm", Replacement: "{1} centimeter",
			},
		})
		if err != nil {
			t.Fatalf("set rule with an upper-case language: %v", err)
		}

		gotRule, err := stack.Rules.GetRule(ctx,
			&spellapi.GetRuleRequest{Id: rule.Id})
		if err != nil {
			t.Fatalf("get rule: %v", err)
		}

		if gotRule.Rule.Language != "sv-se" {
			t.Errorf("stored rule language %q, want sv-se",
				gotRule.Rule.Language)
		}

		unknown := []struct {
			name string
			call func() error
		}{
			{"delete entry", func() error {
				_, err := stack.Dictionaries.DeleteEntry(ctx,
					&spellapi.DeleteEntryRequest{Language: "zz-zz", Text: text})

				return err
			}},
			{"rename entry", func() error {
				_, err := stack.Dictionaries.RenameEntry(ctx,
					&spellapi.RenameEntryRequest{
						Language: "zz-zz", Text: text, NewText: "x",
					})

				return err
			}},
			{"set entry status", func() error {
				_, err := stack.Dictionaries.SetEntryStatus(ctx,
					&spellapi.SetEntryStatusRequest{
						Language: "zz-zz", Text: text, Status: "accepted",
					})

				return err
			}},
			{"set rule", func() error {
				_, err := stack.Rules.SetRule(ctx, &spellapi.SetRuleRequest{
					Rule: &spellapi.Rule{
						Language: "zz-zz", Name: "x", Status: "accepted",
						Pattern: "{digit} cm",
					},
				})

				return err
			}},
		}

		for _, tc := range unknown {
			t.Run(tc.name, func(t *testing.T) {
				err := tc.call()

				assertCode(t, err, connect.CodeInvalidArgument)

				if !strings.Contains(err.Error(), "unsupported language") {
					t.Errorf("error does not name the language: %v", err)
				}
			})
		}
	})
}

// assertCode checks the RPC code without caring which stack produced it.
// The suite runs against both, and a Twirp client returns a twirp.Error
// where a Connect client returns a *connect.Error for the same refusal —
// so an assertion written against either type passes on one stack and
// fails on the other for no reason the service is responsible for.
//
// The want is a connect.Code because that is what the handlers are written
// in; the twirp branch maps back through the same table elephantine's
// interceptor translates forward with.
func assertCode(t *testing.T, err error, want connect.Code) {
	t.Helper()

	if err == nil {
		t.Fatalf("expected a %s error, got nil", want)
	}

	var (
		cErr  *connect.Error
		twErr twirp.Error
	)

	switch {
	case errors.As(err, &cErr):
		if cErr.Code() != want {
			t.Fatalf("expected code %s, got %s: %v", want, cErr.Code(), err)
		}
	case errors.As(err, &twErr):
		got := twirpCodeToConnect(twErr.Code())
		if got != want {
			t.Fatalf("expected code %s, got %s (twirp %s): %v",
				want, got, twErr.Code(), err)
		}
	default:
		t.Fatalf("expected a coded RPC error, got %T: %v", err, err)
	}
}

// twirpCodeToConnect maps the Twirp codes this suite asserts on back to
// the Connect codes the handlers raise. Only the codes the tests use are
// listed; an unexpected one fails loudly rather than comparing as equal to
// something it is not.
func twirpCodeToConnect(code twirp.ErrorCode) connect.Code {
	switch code {
	case twirp.InvalidArgument, twirp.Malformed:
		return connect.CodeInvalidArgument
	case twirp.NotFound:
		return connect.CodeNotFound
	case twirp.AlreadyExists:
		return connect.CodeAlreadyExists
	case twirp.Unauthenticated:
		return connect.CodeUnauthenticated
	case twirp.PermissionDenied:
		return connect.CodePermissionDenied
	case twirp.Internal:
		return connect.CodeInternal
	default:
		return connect.Code(0)
	}
}
