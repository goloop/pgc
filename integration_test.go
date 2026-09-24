//go:build integration

package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goloop/pgc/internal/migrate"
	"github.com/goloop/pgc/internal/pgwire"
)

// These tests run against the server PGC_DATABASE_URL names. Each creates a
// database of its own and drops it afterwards; point the variable at a
// disposable server only.

func adminURL(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("PGC_DATABASE_URL")
	if dsn == "" {
		t.Skip("PGC_DATABASE_URL is not set")
	}
	return dsn
}

func connect(t *testing.T, dsn string) *pgwire.Conn {
	t.Helper()
	cfg, err := pgwire.ParseURL(dsn)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := pgwire.Dial(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// freshDB creates an empty database and returns a connection to it and its
// URL.
func freshDB(t *testing.T) (*pgwire.Conn, string) {
	t.Helper()
	admin := connect(t, adminURL(t))
	name := "pgc_it_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
	if err := admin.Exec("CREATE DATABASE " + name); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	u, _ := url.Parse(adminURL(t))
	u.Path = "/" + name
	c := connect(t, u.String())
	t.Cleanup(func() {
		c.Close()
		admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
		admin.Close()
	})
	return c, u.String()
}

func files(t *testing.T, m map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range m {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func one(t *testing.T, c *pgwire.Conn, sql string) string {
	t.Helper()
	rows, err := c.Query(sql)
	if err != nil {
		t.Fatal(err)
	}
	return rows[0][0].S
}

func TestIntegrationFailedFileLeavesNothing(t *testing.T) {
	c, _ := freshDB(t)
	dir := files(t, map[string]string{"001.sql": "CREATE TABLE t(id int); SELECT 1/0;"})
	if _, err := migrate.Run(c, dir, migrate.Options{}); err == nil {
		t.Fatal("want division by zero")
	}
	if one(t, c, "SELECT to_regclass('public.t') IS NULL AND "+
		"(SELECT count(*) FROM public.pgc_migrations) = 0") != "t" {
		t.Fatal("a failed file left something behind")
	}
}

func TestIntegrationRollbackInAFileIsRefused(t *testing.T) {
	c, _ := freshDB(t)
	dir := files(t, map[string]string{"001.sql": "CREATE TABLE rolled_back(id int); ROLLBACK;"})
	if _, err := migrate.Run(c, dir, migrate.Options{}); err == nil {
		t.Fatal("a file with ROLLBACK was applied")
	}
	if one(t, c, "SELECT to_regclass('public.pgc_migrations') IS NULL") != "t" {
		t.Fatal("the database was touched before the file was checked")
	}
}

func TestIntegrationProcedureThatCommitsIsCaught(t *testing.T) {
	c, _ := freshDB(t)
	if err := c.Exec(`CREATE PROCEDURE p() LANGUAGE plpgsql AS $$
BEGIN CREATE TABLE made(id int); COMMIT; END $$`); err != nil {
		t.Fatal(err)
	}
	dir := files(t, map[string]string{"001.sql": "CALL p();"})
	if _, err := migrate.Run(c, dir, migrate.Options{}); err == nil {
		t.Fatal("want an error")
	}
	if one(t, c, "SELECT count(*) FROM public.pgc_migrations") != "0" {
		t.Fatal("the file was recorded")
	}
}

func TestIntegrationNoTxFailureIsNotRepeated(t *testing.T) {
	c, _ := freshDB(t)
	c.Exec("CREATE TABLE counter(n int)")
	dir := files(t, map[string]string{
		"001.sql": "-- pgc: no-transaction\nINSERT INTO counter VALUES (1); SELECT 1/0;",
	})
	for range 2 {
		if _, err := migrate.Run(c, dir, migrate.Options{}); err == nil {
			t.Fatal("want failure")
		}
	}
	if n := one(t, c, "SELECT count(*) FROM counter"); n != "1" {
		t.Fatalf("the file's first statement ran %s times", n)
	}
	list, err := migrate.Status(c, dir)
	if err != nil || len(list) != 1 || list[0].State != "failed" {
		t.Fatalf("status = %+v %v", list, err)
	}

	// Fix the file, retry it from the top.
	os.WriteFile(filepath.Join(dir, "001.sql"),
		[]byte("-- pgc: no-transaction\nINSERT INTO counter VALUES (2);"), 0o644)
	if err := migrate.Resolve(c, dir, "001.sql", "retry"); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Run(c, dir, migrate.Options{}); err != nil {
		t.Fatal(err)
	}
	if one(t, c, "SELECT state FROM public.pgc_migrations") != "applied" {
		t.Fatal("not recorded as applied")
	}
}

func TestIntegrationStatusOnlyReads(t *testing.T) {
	c, _ := freshDB(t)
	dir := files(t, map[string]string{"001.sql": "SELECT 1;"})
	if _, err := migrate.Status(c, dir); err != nil {
		t.Fatal(err)
	}
	if one(t, c, "SELECT to_regclass('public.pgc_migrations') IS NULL") != "t" {
		t.Fatal("status created the history table")
	}
	if _, err := migrate.Run(c, dir, migrate.Options{}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "001.sql"), []byte("SELECT 2;"), 0o644)
	list, err := migrate.Status(c, dir)
	if err != nil || list[0].State != "changed" {
		t.Fatalf("status = %+v %v", list, err)
	}
}

func TestIntegrationDriftStopsTheRun(t *testing.T) {
	c, _ := freshDB(t)
	dir := files(t, map[string]string{"001.sql": "SELECT 1;"})
	if _, err := migrate.Run(c, dir, migrate.Options{}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "001.sql"), []byte("SELECT 2;"), 0o644)
	os.WriteFile(filepath.Join(dir, "002.sql"), []byte("CREATE TABLE after_drift(id int);"), 0o644)
	if _, err := migrate.Run(c, dir, migrate.Options{}); err == nil {
		t.Fatal("drift did not stop the run")
	}
	if one(t, c, "SELECT to_regclass('public.after_drift') IS NULL") != "t" {
		t.Fatal("a pending file ran despite the drift")
	}
}

func TestIntegrationLockCannotBeShadowed(t *testing.T) {
	c, _ := freshDB(t)
	c.Exec("CREATE FUNCTION public.pg_advisory_lock(integer) RETURNS boolean " +
		"LANGUAGE sql AS 'SELECT true'")
	dir := files(t, map[string]string{"001.sql": "CREATE TABLE lock_check AS " +
		"SELECT count(*) n FROM pg_locks WHERE pid = pg_backend_pid() AND locktype = 'advisory';"})
	if _, err := migrate.Run(c, dir, migrate.Options{}); err != nil {
		t.Fatal(err)
	}
	if n := one(t, c, "SELECT n FROM lock_check"); n != "1" {
		t.Fatalf("the run held %s advisory locks, want 1", n)
	}
}

func TestIntegrationLockTimeout(t *testing.T) {
	c, dsn := freshDB(t)
	other := connect(t, dsn)
	defer other.Close()
	if _, err := other.Query("SELECT pg_advisory_lock(7366499)"); err != nil {
		t.Fatal(err)
	}
	dir := files(t, map[string]string{"001.sql": "SELECT 1;"})
	start := time.Now()
	_, err := migrate.Run(c, dir, migrate.Options{LockTimeout: 200 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("waited %v", d)
	}
}

func TestIntegrationSessionDoesNotLeakBetweenFiles(t *testing.T) {
	c, _ := freshDB(t)
	dir := files(t, map[string]string{
		"001.sql": "CREATE SCHEMA alternate; SET search_path TO alternate;",
		"002.sql": "CREATE TABLE where_am_i(id int);",
	})
	if _, err := migrate.Run(c, dir, migrate.Options{}); err != nil {
		t.Fatal(err)
	}
	if one(t, c, "SELECT to_regclass('public.where_am_i') IS NOT NULL") != "t" {
		t.Fatal("the second file inherited the first file's search_path")
	}
}

func TestIntegrationConcurrentRunsApplyOnce(t *testing.T) {
	c, dsn := freshDB(t)
	other := connect(t, dsn)
	defer other.Close()
	dir := files(t, map[string]string{"001.sql": "CREATE TABLE once_only(n int); " +
		"INSERT INTO once_only VALUES (1); SELECT pg_sleep(0.1);"})
	done := make(chan error, 2)
	for _, conn := range []*pgwire.Conn{c, other} {
		go func() { _, err := migrate.Run(conn, dir, migrate.Options{}); done <- err }()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if n := one(t, c, "SELECT count(*) FROM once_only"); n != "1" {
		t.Fatalf("applied %s times", n)
	}
}

func TestIntegrationUpgradesAnOldHistoryTable(t *testing.T) {
	c, _ := freshDB(t)
	c.Exec(`CREATE TABLE public.pgc_migrations (name text PRIMARY KEY,
		hash text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`)
	c.Exec(`INSERT INTO public.pgc_migrations (name, hash) VALUES ('001.sql', 'adopted')`)
	dir := files(t, map[string]string{"001.sql": "SELECT 1;", "002.sql": "SELECT 2;"})
	list, err := migrate.Status(c, dir)
	if err != nil || list[0].State != "applied" || list[1].State != "pending" {
		t.Fatalf("status on an old table = %+v %v", list, err)
	}
	res, err := migrate.Run(c, dir, migrate.Options{})
	if err != nil || len(res.Applied) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestIntegrationBaseline(t *testing.T) {
	c, _ := freshDB(t)
	dir := files(t, map[string]string{
		"001.sql": "CREATE TABLE a(id int);",
		"002.sql": "CREATE TABLE b(id int);",
		"003.sql": "CREATE TABLE c(id int);",
	})
	c.Exec("CREATE TABLE a(id int); CREATE TABLE b(id int);")
	names, err := migrate.Baseline(c, dir, "002.sql")
	if err != nil || len(names) != 2 {
		t.Fatalf("%v %v", names, err)
	}
	res, err := migrate.Run(c, dir, migrate.Options{})
	if err != nil || len(res.Applied) != 1 || res.Applied[0] != "003.sql" {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := migrate.Baseline(c, dir, "003.sql"); err == nil {
		t.Fatal("a baseline over an existing history was accepted")
	}
}

// Every spelling of status, a typo and a stray word must leave the schema
// alone.
func TestIntegrationCLINeverAppliesByAccident(t *testing.T) {
	c, dsn := freshDB(t)
	dir := files(t, map[string]string{"001.sql": "CREATE TABLE accidental(id int);"})
	cfgPath := filepath.Join(t.TempDir(), "pgc.json")
	os.WriteFile(cfgPath, []byte(fmt.Sprintf(`{"migrations":%q}`, dir)), 0o644)
	t.Setenv("PGC_DATABASE_URL", dsn)

	for _, args := range [][]string{
		{"migrate", "status", "-c", cfgPath},
		{"migrate", "-c", cfgPath, "status"},
		{"migrate", "-c", cfgPath, "status", "-d", dsn},
		{"migrate", "-c", cfgPath, "stauts"},
		{"migrate", "-c", cfgPath, "up", "now"},
		{"migrate", "resolve", "-c", cfgPath},
	} {
		run(args) // status exits non-zero here: 001 is pending, not a problem
		if one(t, c, "SELECT to_regclass('public.accidental') IS NULL") != "t" {
			t.Fatalf("%v applied a migration", args)
		}
	}
	if err := run([]string{"migrate", "-c", cfgPath, "up"}); err != nil {
		t.Fatal(err)
	}
	if one(t, c, "SELECT to_regclass('public.accidental') IS NOT NULL") != "t" {
		t.Fatal("migrate up did not apply")
	}
}

// Deleting a query file removes its generated file on the next generate, and
// check fails until then.
func TestIntegrationGenerateRemovesStaleFiles(t *testing.T) {
	_, dsn := freshDB(t)
	dir := files(t, map[string]string{
		"one.sql": "-- name: One :one\nSELECT 1 AS n;",
		"two.sql": "-- name: Two :one\nSELECT 2 AS n;",
	})
	out := filepath.Join(t.TempDir(), "generated")
	os.MkdirAll(out, 0o755)
	os.WriteFile(filepath.Join(out, "extra.go"), []byte("package generated\n"), 0o644)
	cfgPath := filepath.Join(t.TempDir(), "pgc.json")
	os.WriteFile(cfgPath, []byte(fmt.Sprintf(
		`{"queries":%q,"out":%q,"package":"generated"}`, dir, out)), 0o644)
	t.Setenv("PGC_DATABASE_URL", dsn)

	if err := run([]string{"generate", "-c", cfgPath}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"check", "-c", cfgPath}); err != nil {
		t.Fatalf("check right after generate: %v", err)
	}
	os.Remove(filepath.Join(dir, "one.sql"))
	if err := run([]string{"check", "-c", cfgPath}); err == nil {
		t.Fatal("check passed with a stale generated file")
	}
	if err := run([]string{"generate", "-c", cfgPath}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "one.sql.go")); !os.IsNotExist(err) {
		t.Fatal("one.sql.go survived its query file")
	}
	if _, err := os.Stat(filepath.Join(out, "extra.go")); err != nil {
		t.Fatal("a hand-written file was removed")
	}
	if err := run([]string{"check", "-c", cfgPath}); err != nil {
		t.Fatal(err)
	}
}

// A query that reuses a table's full column set but overrides a column's
// nullability gets a row struct that can hold the NULL an outer join returns.
func TestIntegrationOverrideAgainstARealOuterJoin(t *testing.T) {
	c, dsn := freshDB(t)
	c.Exec("CREATE TABLE parent(id int PRIMARY KEY); " +
		"CREATE TABLE child(id int PRIMARY KEY, name text NOT NULL); " +
		"INSERT INTO parent VALUES (1)")
	dir := files(t, map[string]string{"q.sql": "-- name: Child :one\n" +
		"-- override: id nullable\n-- override: name nullable\n" +
		"SELECT child.id, child.name FROM parent LEFT JOIN child ON child.id = parent.id;"})
	out := filepath.Join(t.TempDir(), "db")
	cfgPath := filepath.Join(t.TempDir(), "pgc.json")
	os.WriteFile(cfgPath, []byte(fmt.Sprintf(`{"queries":%q,"out":%q}`, dir, out)), 0o644)
	t.Setenv("PGC_DATABASE_URL", dsn)
	if err := run([]string{"generate", "-c", cfgPath}); err != nil {
		t.Fatal(err)
	}
	src, _ := os.ReadFile(filepath.Join(out, "q.sql.go"))
	if !strings.Contains(string(src), "ID   *int32") || !strings.Contains(string(src), "Name *string") {
		t.Fatalf("the overrides were not applied:\n%s", src)
	}
}
