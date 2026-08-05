package gen

import (
	"go/token"
	"testing"
)

// CamelCase must always yield a valid, exported Go identifier, even from a
// PostgreSQL name that is not a valid Go identifier.
func TestCamelCaseAlwaysValidIdentifier(t *testing.T) {
	cases := map[string]string{
		"user_api_id":  "UserAPIID",
		"2fa_enabled":  "X2faEnabled", // leading digit gets an X prefix
		"user id%":     "UserID",      // stray punctuation dropped, id is an acronym
		"__":           "X",           // nothing usable -> X
		"колонка":      "Колонка",     // rune-safe capitalization, valid in Go
		"first_name":   "FirstName",
		"id":           "ID",
		"ai_generated": "AIGenerated",
	}
	n := NewNamer()
	for in, want := range cases {
		got := n.CamelCase(in)
		if got != want {
			t.Errorf("CamelCase(%q) = %q, want %q", in, got, want)
		}
		if !token.IsIdentifier(got) {
			t.Errorf("CamelCase(%q) = %q is not a valid Go identifier", in, got)
		}
		if !token.IsExported(got) {
			t.Errorf("CamelCase(%q) = %q is not exported", in, got)
		}
	}
}

// paramName must not produce an identifier that starts with a digit.
func TestParamNameLeadingDigit(t *testing.T) {
	got := NewNamer().paramName("2fa_code")
	if !token.IsIdentifier(got) {
		t.Fatalf("paramName(2fa_code) = %q is not a valid identifier", got)
	}
}

// TestNamerInitialisms covers the project-specific words a schema brings with
// it, which no built-in list could guess.
func TestNamerInitialisms(t *testing.T) {
	n := NewNamer("seo", "CDN", " dm ")

	cases := map[string]string{
		"seo_title":    "SEOTitle",
		"cdn_url":      "CDNURL",
		"dm_thread_id": "DMThreadID",

		// Whole words only: a word that merely starts the same is untouched.
		"seoul_office": "SeoulOffice",
		"cdns":         "Cdns",

		// The built-ins are still there.
		"api_id":       "APIID",
		"ai_generated": "AIGenerated",
	}
	for in, want := range cases {
		if got := n.CamelCase(in); got != want {
			t.Errorf("CamelCase(%q) = %q, want %q", in, got, want)
		}
	}

	if got := n.paramName("seo_title"); got != "seoTitle" {
		t.Errorf("paramName(seo_title) = %q, want seoTitle", got)
	}
	if got := n.paramName("cdn_url"); got != "cdnURL" {
		t.Errorf("paramName(cdn_url) = %q, want cdnURL", got)
	}
}

// TestNamerIsAdditive pins the contract: a project adds words, it cannot take
// a built-in away. Two projects with the same schema get the same names.
func TestNamerIsAdditive(t *testing.T) {
	n := NewNamer("seo")
	if got := n.CamelCase("user_id"); got != "UserID" {
		t.Errorf("CamelCase(user_id) = %q, want UserID", got)
	}
	if got := NewNamer().CamelCase("seo_title"); got != "SeoTitle" {
		t.Errorf("without configuration seo is an ordinary word, got %q", got)
	}
	if got := NewNamer("", "  ").CamelCase("user_id"); got != "UserID" {
		t.Errorf("blank entries must be ignored, got %q", got)
	}
}

// TestRenderDefaultsTheNamer checks a caller that leaves Namer unset still
// gets the built-in spelling everywhere rather than a nil dereference.
func TestRenderDefaultsTheNamer(t *testing.T) {
	in := usersInput()
	in.Namer = nil
	if _, err := Render(in); err != nil {
		t.Fatalf("Render with no Namer = %v", err)
	}
}
