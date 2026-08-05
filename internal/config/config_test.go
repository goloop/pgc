package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Queries != "queries" || cfg.Migrations != "migrations" ||
		cfg.Out != "internal/db" || cfg.Package != "db" ||
		cfg.Nullable != "pointer" {
		t.Fatalf("defaults = %+v", cfg)
	}
}

func TestLoadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pgc.json")
	os.WriteFile(path, []byte(`{
		"out": "gen/db", "nullable": "sqlnull",
		"json_tags": true, "interface": true,
		"rename": {"users": "User"}
	}`), 0o644)

	cfg, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Package != "db" || cfg.Nullable != "sqlnull" ||
		!cfg.JSONTags || !cfg.Interface || cfg.Rename["users"] != "User" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestLoadErrors(t *testing.T) {
	// An explicitly named missing file is an error.
	if _, err := Load(filepath.Join(t.TempDir(), "absent.json"), true); err == nil {
		t.Error("want error for explicit missing file")
	}

	path := filepath.Join(t.TempDir(), "pgc.json")
	os.WriteFile(path, []byte(`{"nullable": "magic"}`), 0o644)
	if _, err := Load(path, true); err == nil ||
		!strings.Contains(err.Error(), "nullable") {
		t.Errorf("want nullable error, got %v", err)
	}

	os.WriteFile(path, []byte(`{broken`), 0o644)
	if _, err := Load(path, true); err == nil {
		t.Error("want error for broken json")
	}
}

// TestInitialisms covers the project-specific abbreviation list, including the
// entries that could never match a column and would otherwise sit unused.
func TestInitialisms(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) (Config, error) {
		path := filepath.Join(dir, "pgc.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return Load(path, true)
	}

	cfg, err := write(`{"initialisms": ["seo", "cdn", "dm"]}`)
	if err != nil {
		t.Fatalf("Load = %v", err)
	}
	if len(cfg.Initialisms) != 3 || cfg.Initialisms[0] != "seo" {
		t.Errorf("Initialisms = %v", cfg.Initialisms)
	}

	for _, bad := range []string{`["seo_title"]`, `["a-b"]`, `[""]`, `["x y"]`} {
		if _, err := write(`{"initialisms": ` + bad + `}`); err == nil {
			t.Errorf("initialisms %s was accepted", bad)
		}
	}
}
