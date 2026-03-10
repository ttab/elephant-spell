package internal

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"

	"github.com/ttab/howdah"
)

// Language represents a UI language option.
type Language struct {
	Code string `json:"code"`
	Name string `json:"name"`
	Flag string `json:"flag"`
}

// Languages is a howdah component that provides the "languages" template func.
type Languages struct {
	list []Language
}

// NewLanguages creates a Languages component from the locales FS.
func NewLanguages(locales fs.FS) (*Languages, error) {
	data, err := fs.ReadFile(locales, "languages.json")
	if err != nil {
		return nil, fmt.Errorf("read languages.json: %w", err)
	}

	var list []Language

	err = json.Unmarshal(data, &list)
	if err != nil {
		return nil, fmt.Errorf("parse languages.json: %w", err)
	}

	return &Languages{list: list}, nil
}

func (l *Languages) RegisterRoutes(_ *howdah.PageMux) {}

// GetTemplateFuncs provides the "languages" template function.
func (l *Languages) GetTemplateFuncs() template.FuncMap {
	return template.FuncMap{
		"languages": func() []Language {
			return l.list
		},
	}
}
