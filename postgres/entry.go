package postgres

type EntryData struct {
	Forms         map[string]string `json:"forms"`
	CaseSensitive bool              `json:"case_sensitive,omitempty"`
	Rule          *EntryRule        `json:"rule,omitempty"`
}

// EntryRule holds a pattern rule stored on an entry. When present the entry is
// matched as a token-pattern rule rather than as literal text.
type EntryRule struct {
	Pattern     string   `json:"pattern"`
	Replacement string   `json:"replacement"`
	Before      []string `json:"before,omitempty"`
	After       []string `json:"after,omitempty"`
	NotBefore   []string `json:"not_before,omitempty"`
	NotAfter    []string `json:"not_after,omitempty"`
}
