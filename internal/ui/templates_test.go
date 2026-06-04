package ui

import (
	"html/template"
	"testing"
)

// TestTemplatesParse fails the build if any embedded template has a syntax
// error or references a function missing from funcMap — these otherwise
// only surface as a startup panic in New().
func TestTemplatesParse(t *testing.T) {
	if _, err := template.New("").Funcs(funcMap).ParseFS(templateFS, "templates/*.html"); err != nil {
		t.Fatalf("templates failed to parse: %v", err)
	}
}
