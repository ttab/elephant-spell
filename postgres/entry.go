package postgres

type EntryData struct {
	Forms         map[string]string `json:"forms"`
	CaseSensitive bool              `json:"case_sensitive,omitempty"`
	Before        []string          `json:"before,omitempty"`
	After         []string          `json:"after,omitempty"`
	NotBefore     []string          `json:"not_before,omitempty"`
	NotAfter      []string          `json:"not_after,omitempty"`
}

// RuleData holds the context guards for a pattern rule, stored in the rule
// table's jsonb data column.
type RuleData struct {
	Before        []string `json:"before,omitempty"`
	After         []string `json:"after,omitempty"`
	NotBefore     []string `json:"not_before,omitempty"`
	NotAfter      []string `json:"not_after,omitempty"`
	CaseSensitive bool     `json:"case_sensitive,omitempty"`
}
