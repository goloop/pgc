// Package config loads pgc.json. The connection URL is deliberately not part
// of the file - it lives in PGC_DATABASE_URL (or DATABASE_URL) so credentials
// never end up in a repository.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"unicode"
)

// Config drives one pgc generate run. Every field has a sensible default,
// so a project without pgc.json works out of the box.
type Config struct {
	// Queries is the directory holding the annotated .sql files.
	Queries string `json:"queries"`

	// Migrations is the directory holding the plain-SQL migration files
	// that "pgc migrate" applies.
	Migrations string `json:"migrations"`

	// Out is the directory the generated package is written to.
	Out string `json:"out"`

	// Package is the generated package name; defaults to the base name
	// of Out.
	Package string `json:"package"`

	// Nullable picks how nullable columns are rendered: "pointer" (the
	// default, *string) or "sqlnull" (sql.Null[string]).
	Nullable string `json:"nullable"`

	// Types overrides the Go type used for a PostgreSQL type name, e.g.
	// {"uuid": "string"} or {"numeric": "string"}.
	Types map[string]string `json:"types"`

	// Rename maps table names to Go struct names, e.g. {"users": "User"}
	// or, schema-qualified, {"audit.users": "AuditUser"}. Without an entry
	// the CamelCase of the table name is used as-is; pgc never guesses
	// singular forms.
	Rename map[string]string `json:"rename"`

	// Initialisms are project-specific abbreviations spelled in full caps
	// inside generated identifiers, e.g. ["seo", "cdn"] renders seo_title as
	// SEOTitle. They add to the built-in list (api, id, json, url and the
	// rest); a built-in cannot be removed.
	Initialisms []string `json:"initialisms"`

	// JSONTags adds `json:"column_name"` tags to model and row structs.
	JSONTags bool `json:"json_tags"`

	// Interface emits querier.go with a Querier interface that *Queries
	// satisfies, for callers that want a test double.
	Interface bool `json:"interface"`
}

// Load reads path. A missing file is fine unless explicit is true - the
// defaults describe a conventional layout.
func Load(path string, explicit bool) (Config, error) {
	cfg := Config{
		Queries:    "queries",
		Migrations: "migrations",
		Out:        "internal/db",
		Nullable:   "pointer",
	}

	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist) && !explicit:
		// Defaults only.
	case err != nil:
		return Config{}, fmt.Errorf("config: %w", err)
	default:
		// Unknown keys are errors: a misspelt "nulable" would otherwise
		// leave the default in force without a word.
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return Config{}, fmt.Errorf("config: %s: %w", path, err)
		}
		if dec.More() {
			return Config{}, fmt.Errorf("config: %s: data after the JSON object", path)
		}
	}

	if cfg.Package == "" {
		cfg.Package = filepath.Base(cfg.Out)
	}
	switch cfg.Nullable {
	case "pointer", "sqlnull":
	default:
		return Config{}, fmt.Errorf(
			"config: nullable must be \"pointer\" or \"sqlnull\", got %q",
			cfg.Nullable)
	}
	for _, w := range cfg.Initialisms {
		if !isWord(w) {
			return Config{}, fmt.Errorf(
				"config: initialism %q must be a single word of letters and "+
					"digits; identifiers are split on \"_\", so only whole "+
					"words can match", w)
		}
	}
	return cfg, nil
}

// isWord reports whether s is one alphanumeric word. Generated identifiers are
// split on underscores and punctuation, so anything else in the list could
// never match a column and would sit in the file doing nothing.
func isWord(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
