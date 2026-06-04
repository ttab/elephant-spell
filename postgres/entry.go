package postgres

type EntryData struct {
	Forms         map[string]string `json:"forms"`
	CaseSensitive bool              `json:"case_sensitive,omitempty"`
}
