package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goloop/pgc/internal/gen"
)

func genFile(name, body string) gen.OutFile {
	return gen.OutFile{Name: name, Data: []byte(generatedHeader + "\n\npackage db\n" + body)}
}

// A file pgc generated for a query file that is gone is removed; a hand-written
// file beside it never is.
func TestPlanOutputFindsStaleFilesOnly(t *testing.T) {
	dir := t.TempDir()
	write := func(name, data string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	two := genFile("two.sql.go", "// two\n")
	write("one.sql.go", string(genFile("one.sql.go", "// one\n").Data))
	write("two.sql.go", string(two.Data))
	write("helpers.go", "package db\n\n// written by hand\n")
	write("notes.txt", generatedHeader+"\n")

	plan, err := planOutput(dir, []gen.OutFile{two})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.write) != 0 {
		t.Errorf("an unchanged file would be rewritten: %v", plan.write)
	}
	if len(plan.stale) != 1 || plan.stale[0] != "one.sql.go" {
		t.Fatalf("stale = %v, want [one.sql.go]", plan.stale)
	}

	if err := plan.apply(dir); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"one.sql.go": false, "two.sql.go": true, "helpers.go": true, "notes.txt": true,
	} {
		_, err := os.Stat(filepath.Join(dir, name))
		if got := err == nil; got != want {
			t.Errorf("%s exists = %v, want %v", name, got, want)
		}
	}
}

// check reports every difference from the directory: a changed file, a
// missing one, a stale one.
func TestPlanOutputReportsDifferences(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.sql.go"), genFile("a.sql.go", "// old\n").Data, 0o644)
	os.WriteFile(filepath.Join(dir, "gone.sql.go"), genFile("gone.sql.go", "").Data, 0o644)

	plan, err := planOutput(dir, []gen.OutFile{
		genFile("a.sql.go", "// new\n"), genFile("b.sql.go", ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := plan.describe(dir)
	for _, want := range []string{"a.sql.go", "b.sql.go", "stale:       " + filepath.Join(dir, "gone.sql.go")} {
		if !strings.Contains(msg, want) {
			t.Errorf("description misses %q:\n%s", want, msg)
		}
	}
	if plan.empty() {
		t.Fatal("plan should not be empty")
	}
}

// An atomic write leaves no temporary file behind.
func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.go")
	for _, data := range []string{"first", "second"} {
		if err := writeFileAtomic(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := os.ReadFile(path)
	entries, _ := os.ReadDir(dir)
	if string(got) != "second" || len(entries) != 1 {
		t.Fatalf("content %q, entries %d", got, len(entries))
	}
}
