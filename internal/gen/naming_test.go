package gen

import (
	"go/token"
	"testing"
)

// CamelCase must always yield a valid, exported Go identifier, even from a
// PostgreSQL name that is not a valid Go identifier.
func TestCamelCaseAlwaysValidIdentifier(t *testing.T) {
	cases := map[string]string{
		"user_api_id": "UserAPIID",
		"2fa_enabled": "X2faEnabled", // leading digit gets an X prefix
		"user id%":    "UserID",      // stray punctuation dropped, id is an acronym
		"__":          "X",           // nothing usable -> X
		"колонка":     "Колонка",     // rune-safe capitalization, valid in Go
		"first_name":  "FirstName",
		"id":          "ID",
	}
	for in, want := range cases {
		got := CamelCase(in)
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
	got := paramName("2fa_code")
	if !token.IsIdentifier(got) {
		t.Fatalf("paramName(2fa_code) = %q is not a valid identifier", got)
	}
}
