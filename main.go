// Command pgc compiles annotated SQL queries into a type-safe Go package
// for PostgreSQL, using a live development database as its type oracle:
// every statement is prepared and described over the wire protocol - never
// executed and never parsed by pgc itself - so parameter and column types
// come from the server that will run them.
//
// The generate command writes the package described by pgc.json; check runs
// the same compilation without writing, for CI; describe prints what the
// server reports about a single statement. The connection URL comes from
// PGC_DATABASE_URL, DATABASE_URL or the -d flag.
//
// What the server said is recorded in pgc.lock.json, and generation falls back
// to that record when no URL is configured - so a fresh clone, a CI job and a
// container build produce the same package without a PostgreSQL to talk to.
// The verify command reports when the record and the database disagree.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goloop/pgc/internal/catalog"
	"github.com/goloop/pgc/internal/compile"
	"github.com/goloop/pgc/internal/config"
	"github.com/goloop/pgc/internal/migrate"
	"github.com/goloop/pgc/internal/pgwire"
	"github.com/goloop/pgc/internal/snapshot"
)

const version = "0.7.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pgc:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return fmt.Errorf("no command")
	}
	switch args[0] {
	case "generate":
		return generateCmd(args[1:], true)
	case "check":
		return generateCmd(args[1:], false)
	case "verify":
		return verifyCmd(args[1:])
	case "migrate":
		return migrateCmd(args[1:])
	case "describe":
		return describeCmd(args[1:])
	case "version":
		fmt.Println("pgc", version)
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `pgc - SQL to Go compiler for PostgreSQL

Usage:
  pgc generate [-c pgc.json] [-d url]  compile the queries into a Go package
  pgc check    [-c pgc.json] [-d url]  compile without writing, for CI
  pgc verify   [-c pgc.json] [-d url]  check pgc.lock.json against the database
  pgc migrate  [-c pgc.json] [-d url]  apply pending migrations, in order
  pgc migrate status                   list applied and pending migrations
  pgc describe [-d url] "SELECT ..."   print parameter and column types
  pgc version                          print the version

The database URL comes from -d, PGC_DATABASE_URL or DATABASE_URL:
  postgres://user:password@host:5432/dbname?sslmode=disable

generate records what the server said in pgc.lock.json. Both generate and check
fall back to that record when no URL is set - so a fresh clone, CI and a
container build need no PostgreSQL. Commit the file, and let pgc verify tell
you when it drifts.
`)
}

// generateCmd runs the compiler; with write=false it only verifies.
//
// With a database URL it works the way it always has, and generate records
// what the server said in pgc.lock.json. Without one, both commands read that
// record instead of connecting, so a fresh clone, a CI job or a container
// build needs no PostgreSQL. Only generate writes the record: check is the
// command that writes nothing.
func generateCmd(args []string, write bool) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	cfgPath := fs.String("c", "pgc.json", "config file")
	dsn := fs.String("d", "", "database url (default: $PGC_DATABASE_URL)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(fs, *cfgPath)
	if err != nil {
		return err
	}

	queries, err := compile.Queries(cfg)
	if err != nil {
		return err
	}

	var (
		ds       []compile.Described
		cat      *catalog.Catalog
		warnings []string
		lock     *snapshot.File
	)
	if conn, err := dial(*dsn); err == nil {
		defer conn.Close()
		if ds, cat, err = compile.Describe(conn, queries); err != nil {
			return err
		}
		if lock, err = snapshot.Of(version, cfg.Migrations, ds, cat); err != nil {
			return err
		}
	} else if !errors.Is(err, errNoDatabase) {
		return err
	} else {
		path := snapshot.Path(*cfgPath)
		f, lerr := snapshot.Load(path)
		if lerr != nil {
			if errors.Is(lerr, iofs.ErrNotExist) {
				return fmt.Errorf("%w, and no %s to generate from; run pgc "+
					"generate once with a database to write it", err, snapshot.Name)
			}
			return lerr
		}
		if ds, cat, err = f.Describe(queries); err != nil {
			return err
		}
		warnings = append(warnings, f.Warnings(cfg.Migrations)...)
	}

	res, err := compile.Build(cfg, ds, cat)
	if err != nil {
		return err
	}
	res.Warnings = append(warnings, res.Warnings...)
	for _, w := range res.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}

	if !write {
		fmt.Printf("ok: %d file(s) compile cleanly\n", len(res.Files))
		return nil
	}

	if err := os.MkdirAll(cfg.Out, 0o755); err != nil {
		return err
	}
	for _, f := range res.Files {
		path := filepath.Join(cfg.Out, f.Name)
		if err := os.WriteFile(path, f.Data, 0o644); err != nil {
			return err
		}
		fmt.Println(path)
	}

	if lock != nil {
		path := snapshot.Path(*cfgPath)
		if err := snapshot.Save(path, lock); err != nil {
			return err
		}
		fmt.Printf("%s (%s)\n", snapshot.TrimPath(path), lock.Summary())
	}
	return nil
}

// verifyCmd checks the recorded snapshot still matches the database. It is the
// other half of generating without one: the record is only worth committing if
// something notices when it stops being true.
func verifyCmd(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	cfgPath := fs.String("c", "pgc.json", "config file")
	dsn := fs.String("d", "", "database url (default: $PGC_DATABASE_URL)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(fs, *cfgPath)
	if err != nil {
		return err
	}

	path := snapshot.Path(*cfgPath)
	recorded, err := snapshot.Load(path)
	if err != nil {
		return err
	}

	conn, err := dial(*dsn)
	if err != nil {
		return err
	}
	defer conn.Close()

	queries, err := compile.Queries(cfg)
	if err != nil {
		return err
	}
	ds, cat, err := compile.Describe(conn, queries)
	if err != nil {
		return err
	}
	fresh, err := snapshot.Of(version, cfg.Migrations, ds, cat)
	if err != nil {
		return err
	}

	diffs := recorded.Compare(fresh)
	if len(diffs) == 0 {
		fmt.Printf("ok: %s matches the database (%s)\n",
			snapshot.TrimPath(path), fresh.Summary())
		return nil
	}
	for _, d := range diffs {
		fmt.Fprintln(os.Stderr, " ", d)
	}
	return fmt.Errorf("%s is out of date; run pgc generate against the database",
		snapshot.TrimPath(path))
}

// loadConfig reads the configuration, remembering whether -c was given so a
// missing file is only an error when one was asked for by name.
func loadConfig(fs *flag.FlagSet, path string) (config.Config, error) {
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "c" {
			explicit = true
		}
	})
	return config.Load(path, explicit)
}

// migrateCmd applies pending migrations or reports their status.
func migrateCmd(args []string) error {
	status := false
	if len(args) > 0 && args[0] == "status" {
		status = true
		args = args[1:]
	}

	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	cfgPath := fs.String("c", "pgc.json", "config file")
	dsn := fs.String("d", "", "database url (default: $PGC_DATABASE_URL)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "c" {
			explicit = true
		}
	})
	cfg, err := config.Load(*cfgPath, explicit)
	if err != nil {
		return err
	}

	conn, err := dial(*dsn)
	if err != nil {
		return err
	}
	defer conn.Close()

	// A migration statement may legitimately run for a long time (index
	// builds, backfills); wait for as long as the server needs.
	conn.SetOpTimeout(0)

	if status {
		list, warnings, err := migrate.Status(conn, cfg.Migrations)
		if err != nil {
			return err
		}
		for _, m := range list {
			if m.Applied {
				fmt.Printf("applied  %-40s %s\n", m.Name, m.AppliedAt)
				continue
			}
			fmt.Printf("pending  %s\n", m.Name)
		}
		for _, w := range warnings {
			fmt.Fprintln(os.Stderr, "warning:", w)
		}
		return nil
	}

	res, err := migrate.Run(conn, cfg.Migrations)
	if res != nil {
		for _, name := range res.Applied {
			fmt.Println("applied", name)
		}
		for _, w := range res.Warnings {
			fmt.Fprintln(os.Stderr, "warning:", w)
		}
	}
	if err != nil {
		return err
	}
	if len(res.Applied) == 0 {
		fmt.Println("nothing to apply")
	}
	return nil
}

// dial resolves the connection URL and connects.
// errNoDatabase says no connection was configured, which for generation is a
// reason to look for a snapshot rather than a failure.
var errNoDatabase = errors.New(
	"no database url: set PGC_DATABASE_URL or pass -d")

func dial(dsn string) (*pgwire.Conn, error) {
	if dsn == "" {
		dsn = os.Getenv("PGC_DATABASE_URL")
	}
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		return nil, errNoDatabase
	}
	cfg, err := pgwire.ParseURL(dsn)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return pgwire.Dial(ctx, cfg)
}

func describeCmd(args []string) error {
	fs := flag.NewFlagSet("describe", flag.ContinueOnError)
	dsn := fs.String("d", "", "database url (default: $PGC_DATABASE_URL)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("describe expects exactly one SQL argument")
	}
	query := fs.Arg(0)

	conn, err := dial(*dsn)
	if err != nil {
		return err
	}
	defer conn.Close()

	st, err := conn.Describe(query)
	if err != nil {
		return err
	}

	names, err := typeNames(conn, st)
	if err != nil {
		return err
	}
	tables, notNull, err := columnOrigins(conn, st.Columns)
	if err != nil {
		return err
	}

	fmt.Printf("server %s\n\n", conn.Parameter("server_version"))

	fmt.Println("Parameters:")
	if len(st.ParamOIDs) == 0 {
		fmt.Println("  (none)")
	}
	for i, oid := range st.ParamOIDs {
		fmt.Printf("  $%-3d %s\n", i+1, names[oid])
	}

	fmt.Println("\nColumns:")
	if len(st.Columns) == 0 {
		fmt.Println("  (statement returns no rows)")
	}
	for _, col := range st.Columns {
		null := "nullable"
		origin := "(expression)"
		if col.TableOID != 0 {
			origin = fmt.Sprintf("%s.%s", tables[col.TableOID], col.Name)
			if notNull[originKey{col.TableOID, col.Attnum}] {
				null = "not null"
			}
		}
		fmt.Printf("  %-20s %-12s %-9s %s\n", col.Name, names[col.TypeOID], null, origin)
	}
	return nil
}

// typeNames resolves every OID used by the statement to its pg_type name.
func typeNames(conn *pgwire.Conn, st *pgwire.Statement) (map[uint32]string, error) {
	oids := map[uint32]bool{}
	for _, oid := range st.ParamOIDs {
		oids[oid] = true
	}
	for _, col := range st.Columns {
		oids[col.TypeOID] = true
	}
	names := map[uint32]string{}
	if len(oids) == 0 {
		return names, nil
	}

	rows, err := conn.Query(
		"SELECT oid, typname FROM pg_catalog.pg_type WHERE oid IN (" +
			joinOIDs(oids) + ")")
	if err != nil {
		return nil, fmt.Errorf("type lookup: %w", err)
	}
	for _, row := range rows {
		var oid uint32
		fmt.Sscanf(row[0].S, "%d", &oid)
		names[oid] = row[1].S
	}
	return names, nil
}

type originKey struct {
	table  uint32
	attnum int16
}

// columnOrigins resolves table names and attnotnull for every column that
// has a table origin.
func columnOrigins(
	conn *pgwire.Conn,
	cols []pgwire.Column,
) (map[uint32]string, map[originKey]bool, error) {
	tables := map[uint32]string{}
	notNull := map[originKey]bool{}

	tableOIDs := map[uint32]bool{}
	var pairs []string
	for _, col := range cols {
		if col.TableOID == 0 {
			continue
		}
		tableOIDs[col.TableOID] = true
		pairs = append(pairs, fmt.Sprintf("(%d,%d)", col.TableOID, col.Attnum))
	}
	if len(tableOIDs) == 0 {
		return tables, notNull, nil
	}

	rows, err := conn.Query(
		"SELECT oid, relname FROM pg_catalog.pg_class WHERE oid IN (" +
			joinOIDs(tableOIDs) + ")")
	if err != nil {
		return nil, nil, fmt.Errorf("table lookup: %w", err)
	}
	for _, row := range rows {
		var oid uint32
		fmt.Sscanf(row[0].S, "%d", &oid)
		tables[oid] = row[1].S
	}

	rows, err = conn.Query(
		"SELECT attrelid, attnum, attnotnull FROM pg_catalog.pg_attribute " +
			"WHERE (attrelid, attnum) IN (" + strings.Join(pairs, ",") + ")")
	if err != nil {
		return nil, nil, fmt.Errorf("nullability lookup: %w", err)
	}
	for _, row := range rows {
		var table uint32
		var attnum int16
		fmt.Sscanf(row[0].S, "%d", &table)
		fmt.Sscanf(row[1].S, "%d", &attnum)
		notNull[originKey{table, attnum}] = row[2].S == "t"
	}
	return tables, notNull, nil
}

// joinOIDs renders a set of OIDs as "1,2,3" for an IN list. OIDs are server
// integers, never user input.
func joinOIDs(oids map[uint32]bool) string {
	list := make([]string, 0, len(oids))
	for oid := range oids {
		list = append(list, fmt.Sprint(oid))
	}
	sort.Strings(list)
	return strings.Join(list, ",")
}
