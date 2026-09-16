package internal

// The string values the web UI shares with the templates and the API. They are
// named here rather than repeated as literals so that a template and the
// handler feeding it cannot drift apart on a spelling.

// Correction levels, as the API spells spell.CorrectionLevel and the templates
// read it.
const (
	uiLevelError      = "error"
	uiLevelSuggestion = "suggestion"
)

// Moderation statuses. An entry or rule starts as pending so that additions go
// through moderation before taking effect.
const statusPending = "pending"

// Flash message types, which the templates turn into a style.
const (
	flashError   = "error"
	flashSuccess = "success"
)

// Page templates.
const (
	tmplDictionaries = "dictionaries.html"
	tmplEntryForm    = "entry_form.html"
	tmplRules        = "rules.html"
	tmplRuleForm     = "rule_form.html"
)

// Fragment templates, rendered on their own in response to a form post or a
// live-preview request.
const (
	tmplEntryResponse  = "entry_response.html"
	tmplEntryRename    = "entry_rename.html"
	tmplEntryList      = "entry_list.html"
	tmplPatternPreview = "pattern_preview.html"
	tmplExpansions     = "expansions.html"
	tmplRuleResponse   = "rule_response.html"
	tmplRuleList       = "rule_list.html"
	tmplRuleTest       = "rule_test.html"
)
