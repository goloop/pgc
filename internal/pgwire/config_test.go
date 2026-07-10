package pgwire

import "testing"

func TestParseURL(t *testing.T) {
	cfg, err := ParseURL("postgres://alice:s3cret@db.example.com:6543/app?sslmode=require")
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Host: "db.example.com", Port: "6543",
		User: "alice", Password: "s3cret",
		Database: "app", SSLMode: "require",
	}
	if cfg != want {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestParseURLDefaults(t *testing.T) {
	cfg, err := ParseURL("postgresql://bob@localhost")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != "5432" || cfg.Database != "bob" || cfg.SSLMode != "prefer" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestParseURLErrors(t *testing.T) {
	for _, dsn := range []string{
		"mysql://u@h/db",                    // wrong scheme
		"postgres://u@h/db?sslmode=magic",   // unknown sslmode
		"postgres://h/db",                   // no user
		"postgres://u:p@h:port-not-number/", // bad url
	} {
		if _, err := ParseURL(dsn); err == nil {
			t.Errorf("ParseURL(%q): want error", dsn)
		}
	}
}
