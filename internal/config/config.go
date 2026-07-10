// Package config loads pgc.json. The connection URL is deliberately not part
// of the file - it lives in PGC_DATABASE_URL (or DATABASE_URL) so credentials
// never end up in a repository.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Config drives one pgc generate run. Every field has a sensible default,
// so a project without pgc.json works out of the box.
type Config struct {
	// Queries is the directory holding the annotated .sql files.
	Queries string `json:"queries"`

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

	// Rename maps table names to Go struct names, e.g. {"users": "User"}.
	// Without an entry the CamelCase of the table name is used as-is;
	// pgc never guesses singular forms.
	Rename map[string]string `json:"rename"`
}

// Load reads path. A missing file is fine unless explicit is true - the
// defaults describe a conventional layout.
func Load(path string, explicit bool) (Config, error) {
	cfg := Config{
		Queries:  "queries",
		Out:      "internal/db",
		Nullable: "pointer",
	}

	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist) && !explicit:
		// Defaults only.
	case err != nil:
		return Config{}, fmt.Errorf("config: %w", err)
	default:
		if err := json.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("config: %s: %w", path, err)
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
	return cfg, nil
}
