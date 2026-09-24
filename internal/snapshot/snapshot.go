// Package snapshot records what the database said about a set of queries, so
// a later run can generate the same code without one.
//
// Generation asks a live PostgreSQL to describe every statement: that is where
// the types come from, and there is no substitute for it. What there can be is
// a written record of the answers. With one committed alongside the queries,
// a fresh clone, a CI job and a container build produce the generated package
// without a server, and the record itself becomes the visible contract between
// the migrations, the .sql files and the Go that comes out.
//
// The record is only ever trusted when it still matches the queries it was
// taken for; anything else is reported as stale rather than used.
package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/goloop/pgc/internal/catalog"
	"github.com/goloop/pgc/internal/compile"
	"github.com/goloop/pgc/internal/pgwire"
	"github.com/goloop/pgc/internal/queryfile"
)

// Name is the file a snapshot is written to, beside the configuration.
const Name = "pgc.lock.json"

// version is the layout of the file. A snapshot written by an older pgc is
// refused rather than half understood.
const version = 1

// File is the whole record: one entry per query, plus the slice of the system
// catalogs those queries touch.
type File struct {
	Version int    `json:"version"`
	PGC     string `json:"pgc"`

	// Migrations fingerprints the migration files this schema was read
	// through, so a snapshot taken before a migration was written can say so.
	// Empty when there is no migrations directory.
	Migrations string `json:"migrations,omitempty"`

	Queries []Query          `json:"queries"`
	Catalog catalog.Snapshot `json:"catalog"`
}

// Query is what the server said about one statement. SQL is a fingerprint,
// not the text: the text lives in the .sql file, and this only has to notice
// when the two stop agreeing.
type Query struct {
	File    string          `json:"file"`
	Name    string          `json:"name"`
	SQL     string          `json:"sql"`
	Params  []uint32        `json:"params,omitempty"`
	Columns []pgwire.Column `json:"columns,omitempty"`
}

// Path returns the snapshot path that belongs to a configuration file.
func Path(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), Name)
}

// Of records a described run.
func Of(pgcVersion, migrationsDir string, ds []compile.Described, cat *catalog.Catalog) (*File, error) {
	f := &File{Version: version, PGC: pgcVersion, Catalog: cat.Snapshot()}

	digest, err := fingerprintDir(migrationsDir)
	if err != nil {
		return nil, err
	}
	f.Migrations = digest

	for _, d := range ds {
		f.Queries = append(f.Queries, Query{
			File:    filepath.ToSlash(d.Query.File),
			Name:    d.Query.Name,
			SQL:     fingerprint([]byte(d.Query.SQL)),
			Params:  d.Statement.ParamOIDs,
			Columns: d.Statement.Columns,
		})
	}
	sort.Slice(f.Queries, func(i, j int) bool {
		if f.Queries[i].File != f.Queries[j].File {
			return f.Queries[i].File < f.Queries[j].File
		}
		return f.Queries[i].Name < f.Queries[j].Name
	})
	return f, nil
}

// Save writes the snapshot, indented and in a fixed order so that a run
// against an unchanged schema leaves the file byte for byte as it was.
func Save(path string, f *File) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	return writeFileAtomic(path, append(data, '\n'), 0o644)
}

// writeFileAtomic replaces path with data in one rename, so an interrupted
// run never leaves a record cut short.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("snapshot: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	if err := os.Chmod(name, perm); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	return nil
}

// Load reads a snapshot.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("snapshot: %s: %w", path, err)
	}
	if f.Version != version {
		return nil, fmt.Errorf(
			"snapshot: %s was written by a different pgc (format %d, this is %d); "+
				"run pgc generate against the database to rewrite it",
			path, f.Version, version)
	}

	// The file is looked up by query, so a repeated one would quietly win.
	// A record that contradicts itself is not a record.
	seen := map[string]bool{}
	for _, q := range f.Queries {
		k := key(q.File, q.Name)
		if seen[k] {
			return nil, fmt.Errorf(
				"snapshot: %s records %s in %s twice; rewrite it with pgc "+
					"generate against the database", path, q.Name, q.File)
		}
		seen[k] = true
	}
	return &f, nil
}

// Describe replays the recorded answers for queries, in the shape the
// compiler expects. Every query must be in the snapshot with the SQL it was
// recorded for; one that is not makes the whole snapshot stale, because the
// types it would need were never read.
func (f *File) Describe(queries []queryfile.Query) ([]compile.Described, *catalog.Catalog, error) {
	byKey := make(map[string]Query, len(f.Queries))
	for _, q := range f.Queries {
		byKey[key(q.File, q.Name)] = q
	}

	var ds []compile.Described
	for _, q := range queries {
		rec, ok := byKey[key(filepath.ToSlash(q.File), q.Name)]
		if !ok {
			return nil, nil, staleErr(q, "it is not in the snapshot")
		}
		if rec.SQL != fingerprint([]byte(q.SQL)) {
			return nil, nil, staleErr(q, "its SQL has changed since the snapshot")
		}
		ds = append(ds, compile.Described{
			Query: q,
			Statement: &pgwire.Statement{
				ParamOIDs: rec.Params,
				Columns:   rec.Columns,
			},
		})
	}

	cat, err := catalog.FromSnapshot(f.Catalog)
	if err != nil {
		return nil, nil, err
	}
	return ds, cat, nil
}

// Warnings reports what the snapshot cannot vouch for: a query it has never
// heard of is an error, but migrations written after it was taken are only a
// reason to look. The schema may be exactly right; nothing here can tell.
func (f *File) Warnings(migrationsDir string) []string {
	digest, err := fingerprintDir(migrationsDir)
	if err != nil || digest == f.Migrations {
		return nil
	}
	return []string{fmt.Sprintf(
		"the migrations in %s have changed since %s was written; the recorded "+
			"schema may be out of date - run pgc generate against a migrated "+
			"database to rewrite it", migrationsDir, Name)}
}

// Compare reports how a freshly described run differs from the snapshot. An
// empty result means the file on disk is exactly what the database says now,
// which is what a CI job wants to hear.
func (f *File) Compare(fresh *File) []string {
	var diffs []string

	was := map[string]Query{}
	for _, q := range f.Queries {
		was[key(q.File, q.Name)] = q
	}
	now := map[string]Query{}
	for _, q := range fresh.Queries {
		now[key(q.File, q.Name)] = q
	}

	for _, q := range fresh.Queries {
		k := key(q.File, q.Name)
		old, ok := was[k]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("%s: %s is missing from %s",
				q.File, q.Name, Name))
			continue
		}
		switch {
		case old.SQL != q.SQL:
			diffs = append(diffs, fmt.Sprintf("%s: %s has different SQL",
				q.File, q.Name))
		case !sameColumns(old, q):
			diffs = append(diffs, fmt.Sprintf(
				"%s: %s describes differently now - the schema has moved",
				q.File, q.Name))
		}
	}
	for _, q := range f.Queries {
		if _, ok := now[key(q.File, q.Name)]; !ok {
			diffs = append(diffs, fmt.Sprintf("%s: %s is in %s but no longer exists",
				q.File, q.Name, Name))
		}
	}

	diffs = append(diffs, compareCatalog(f.Catalog, fresh.Catalog)...)

	if f.Migrations != fresh.Migrations {
		diffs = append(diffs, fmt.Sprintf(
			"the migration files have changed since %s was written", Name))
	}
	sort.Strings(diffs)
	return diffs
}

// compareCatalog reports schema movement that no query describes differently
// for. An enum gaining a label is the plain case: every statement still has
// the same parameters and columns, while the generated constants, the Values
// list and the parser all change. Comparing only the queries would call that
// snapshot current.
func compareCatalog(was, now catalog.Snapshot) []string {
	var diffs []string

	names := map[uint32]string{}
	for _, t := range now.Types {
		names[t.OID] = t.Name
	}
	for _, t := range was.Types {
		if _, ok := names[t.OID]; !ok {
			names[t.OID] = t.Name
		}
	}
	name := func(oid uint32) string {
		if n, ok := names[oid]; ok {
			return n
		}
		return fmt.Sprintf("oid %d", oid)
	}

	oldTypes := map[uint32]catalog.Type{}
	for _, t := range was.Types {
		oldTypes[t.OID] = t
	}
	for _, t := range now.Types {
		if old, ok := oldTypes[t.OID]; !ok {
			diffs = append(diffs, fmt.Sprintf("the type %s is new", t.Name))
		} else if old != t {
			diffs = append(diffs, fmt.Sprintf("the type %s has changed", t.Name))
		}
	}
	for _, t := range was.Types {
		if _, ok := names[t.OID]; ok {
			if !containsType(now.Types, t.OID) {
				diffs = append(diffs, fmt.Sprintf("the type %s is gone", t.Name))
			}
		}
	}

	for key, labels := range now.Enums {
		old, ok := was.Enums[key]
		switch {
		case !ok:
			diffs = append(diffs, fmt.Sprintf("the enum %s is new", enumName(key, name)))
		case !equalStrings(old, labels):
			diffs = append(diffs, fmt.Sprintf(
				"the values of enum %s have changed", enumName(key, name)))
		}
	}
	for key := range was.Enums {
		if _, ok := now.Enums[key]; !ok {
			diffs = append(diffs, fmt.Sprintf("the enum %s is gone", enumName(key, name)))
		}
	}

	oldTables := map[uint32]*catalog.Table{}
	for _, t := range was.Tables {
		oldTables[t.OID] = t
	}
	newTables := map[uint32]*catalog.Table{}
	for _, t := range now.Tables {
		newTables[t.OID] = t
	}
	for _, t := range now.Tables {
		old, ok := oldTables[t.OID]
		switch {
		case !ok:
			diffs = append(diffs, fmt.Sprintf("the table %s is new", t.Name))
		case !equalTables(old, t):
			diffs = append(diffs, fmt.Sprintf("the columns of table %s have changed", t.Name))
		}
	}
	for _, t := range was.Tables {
		if _, ok := newTables[t.OID]; !ok {
			diffs = append(diffs, fmt.Sprintf("the table %s is gone", t.Name))
		}
	}

	return diffs
}

// enumName renders an enum's type name from its recorded OID key.
func enumName(key string, name func(uint32) string) string {
	oid, err := strconv.ParseUint(key, 10, 32)
	if err != nil {
		return "oid " + key
	}
	return name(uint32(oid))
}

func containsType(types []catalog.Type, oid uint32) bool {
	for _, t := range types {
		if t.OID == oid {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalTables(a, b *catalog.Table) bool {
	if a.Schema != b.Schema || a.Name != b.Name || len(a.Columns) != len(b.Columns) {
		return false
	}
	for i := range a.Columns {
		if a.Columns[i] != b.Columns[i] {
			return false
		}
	}
	return true
}

// sameColumns reports whether two records describe the same result shape and
// parameters.
func sameColumns(a, b Query) bool {
	if len(a.Params) != len(b.Params) || len(a.Columns) != len(b.Columns) {
		return false
	}
	for i := range a.Params {
		if a.Params[i] != b.Params[i] {
			return false
		}
	}
	for i := range a.Columns {
		if a.Columns[i] != b.Columns[i] {
			return false
		}
	}
	return true
}

// staleErr explains which query the snapshot cannot answer for, and what to do.
func staleErr(q queryfile.Query, why string) error {
	return fmt.Errorf(
		"%s:%d: %s cannot be generated from %s: %s. Run pgc generate with a "+
			"database URL to rewrite the snapshot",
		q.File, q.Line, q.Name, Name, why)
}

// key identifies a query across runs.
func key(file, name string) string { return file + "\x00" + name }

// fingerprint is the digest recorded for a text, short enough to read in a
// diff and long enough not to collide.
func fingerprint(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fingerprintDir digests every file in a directory, names and contents, so
// that adding, editing or removing one shows. A directory that is not there
// digests to nothing, which is what a project without migrations has.
func fingerprintDir(dir string) (string, error) {
	if dir == "" {
		return "", nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("snapshot: %w", err)
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	h := sha256.New()
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", fmt.Errorf("snapshot: %w", err)
		}
		fmt.Fprintf(h, "%s\x00%d\x00", name, len(data))
		h.Write(data)
	}
	if len(names) == 0 {
		return "", nil
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// Summary is a one-line description of a snapshot, for a command that has
// just written one.
func (f *File) Summary() string {
	tables := len(f.Catalog.Tables)
	return fmt.Sprintf("%d quer%s, %d table%s",
		len(f.Queries), plural(len(f.Queries), "y", "ies"),
		tables, plural(tables, "", "s"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// TrimPath shortens a path for a message, so a long absolute one does not
// bury what it is saying.
func TrimPath(p string) string {
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, p); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return p
}
