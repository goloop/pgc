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
	"net"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/goloop/pgc/internal/catalog"
	"github.com/goloop/pgc/internal/compile"
	"github.com/goloop/pgc/internal/config"
	"github.com/goloop/pgc/internal/migrate"
	"github.com/goloop/pgc/internal/pgwire"
	"github.com/goloop/pgc/internal/snapshot"
)

// fallbackVersion is the release this source belongs to, for a binary built
// from a checkout (`go build`, `go run`), where the module has no tag to
// report.
const fallbackVersion = "0.8.3"

// version is what `pgc version` prints and what pgc.lock.json records. A binary
// built with `go install github.com/goloop/pgc@vX.Y.Z` reports that tag, read
// from the build info, so the string cannot fall behind the release the way a
// constant edited by hand does.
var version = buildVersion()

func buildVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return strings.TrimPrefix(v, "v")
		}
	}
	return fallbackVersion
}

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
  pgc check    [-c pgc.json] [-d url]  fail unless the package is up to date
  pgc verify   [-c pgc.json] [-d url]  check the queries' types in pgc.lock.json
                                       against the database
  pgc migrate  [up] [-c pgc.json] [-d url] [-allow-drift]
               [-lock-timeout 10m] [-timeout 0]
                                       apply pending migrations, in order
  pgc migrate status                   list every migration's state; fails
                                       when one needs attention
  pgc migrate resolve <file> applied|retry
                                       settle an unfinished no-transaction file
  pgc migrate baseline <last-file>     record files up to <last-file> as
                                       applied, for an existing schema
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

// generateCmd runs the compiler. generate brings the output directory up to
// date - writing what changed, removing what pgc wrote earlier for queries
// that are gone - and check, with write=false, only reports whether it is.
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

	plan, err := planOutput(cfg.Out, res.Files)
	if err != nil {
		return err
	}
	if !write {
		if !plan.empty() {
			return fmt.Errorf("the generated package differs from what the "+
				"queries produce; run pgc generate:\n%s", plan.describe(cfg.Out))
		}
		fmt.Printf("ok: %d generated file(s) up to date\n", len(res.Files))
		return nil
	}
	if err := plan.apply(cfg.Out); err != nil {
		return err
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
		fmt.Printf("ok: the query types in %s match the database (%s)\n",
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

// migrateCmd dispatches the migrate subcommands. Every argument is checked
// before a connection is opened: an unknown word or a misplaced one must never
// fall through to applying migrations.
func migrateCmd(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	cfgPath := fs.String("c", "pgc.json", "config file")
	dsn := fs.String("d", "", "database url (default: $PGC_DATABASE_URL)")
	allowDrift := fs.Bool("allow-drift", false,
		"apply pending files even when an applied one changed or disappeared")
	lockTimeout := fs.Duration("lock-timeout", 10*time.Minute,
		"how long to wait for another run's lock (0: no limit)")
	timeout := fs.Duration("timeout", 0,
		"cancel the run after this long (0: no limit)")

	// Flags may come before, between or after the words, so the parse
	// resumes after each word.
	var words []string
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		words = append(words, fs.Arg(0))
		args = fs.Args()[1:]
	}

	sub := "up"
	if len(words) > 0 {
		sub, words = words[0], words[1:]
	}
	want := map[string]int{"up": 0, "status": 0, "resolve": 2, "baseline": 1}
	n, ok := want[sub]
	if !ok {
		return fmt.Errorf("unknown migrate command %q (want up, status, "+
			"resolve or baseline)", sub)
	}
	if len(words) != n {
		switch sub {
		case "resolve":
			return fmt.Errorf("usage: pgc migrate resolve <file> applied|retry")
		case "baseline":
			return fmt.Errorf("usage: pgc migrate baseline <last-file>")
		}
		return fmt.Errorf("pgc migrate %s takes no arguments, got %q",
			sub, strings.Join(words, " "))
	}

	cfg, err := loadConfig(fs, *cfgPath)
	if err != nil {
		return err
	}

	conn, target, err := dialTarget(*dsn)
	if err != nil {
		return err
	}
	defer conn.Close()
	fmt.Fprintf(os.Stderr, "pgc: database %s\n", target)

	// A migration statement may legitimately run for a long time (index
	// builds, backfills), so there is no per-request deadline. What bounds a
	// run is -timeout and the operator: either cancels the statement in
	// flight on the server, which leaves the connection able to roll back
	// and release the lock.
	conn.SetOpTimeout(0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "pgc: cancelling the statement in flight")
			conn.Cancel(context.Background())
		case <-finished:
		}
	}()

	switch sub {
	case "status":
		return migrateStatus(conn, cfg.Migrations)
	case "resolve":
		if err := migrate.Resolve(conn, cfg.Migrations, words[0], words[1]); err != nil {
			return err
		}
		fmt.Printf("resolved %s as %s\n", words[0], words[1])
		return nil
	case "baseline":
		names, err := migrate.Baseline(conn, cfg.Migrations, words[0])
		if err != nil {
			return err
		}
		for _, name := range names {
			fmt.Println("baseline", name)
		}
		return nil
	}

	res, err := migrate.Run(conn, cfg.Migrations, migrate.Options{
		AllowDrift:  *allowDrift,
		LockTimeout: *lockTimeout,
	})
	if res != nil {
		for _, name := range res.Applied {
			fmt.Println("applied", name)
		}
		for _, w := range res.Warnings {
			fmt.Fprintln(os.Stderr, "warning:", w)
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%w (cancelled: %v)", err, context.Cause(ctx))
		}
		return err
	}
	if len(res.Applied) == 0 {
		fmt.Println("nothing to apply")
	}
	return nil
}

// migrateStatus prints the state of every migration and fails when any needs
// a person to look at it, so a CI step can gate on it.
func migrateStatus(conn *pgwire.Conn, dir string) error {
	list, err := migrate.Status(conn, dir)
	if err != nil {
		return err
	}
	problems := 0
	for _, m := range list {
		if m.Problem() {
			problems++
		}
		if m.AppliedAt != "" {
			fmt.Printf("%-8s %-40s %s\n", m.State, m.Name, m.AppliedAt)
			continue
		}
		fmt.Printf("%-8s %s\n", m.State, m.Name)
	}
	if problems > 0 {
		return fmt.Errorf("%d migration(s) need attention: changed or missing "+
			"files, or no-transaction files left unfinished", problems)
	}
	return nil
}

// dial resolves the connection URL and connects.
// errNoDatabase says no connection was configured, which for generation is a
// reason to look for a snapshot rather than a failure.
var errNoDatabase = errors.New(
	"no database url: set PGC_DATABASE_URL or pass -d")

func dial(dsn string) (*pgwire.Conn, error) {
	conn, _, err := dialTarget(dsn)
	return conn, err
}

// dialTarget connects and also describes where to: user, host, port and
// database, and which setting named them - never the password. Commands that
// change a database print it first, so a DATABASE_URL meant for something
// else is noticed.
func dialTarget(dsn string) (*pgwire.Conn, string, error) {
	source := "-d"
	if dsn == "" {
		dsn, source = os.Getenv("PGC_DATABASE_URL"), "PGC_DATABASE_URL"
	}
	if dsn == "" {
		dsn, source = os.Getenv("DATABASE_URL"), "DATABASE_URL"
	}
	if dsn == "" {
		return nil, "", errNoDatabase
	}
	cfg, err := pgwire.ParseURL(dsn)
	if err != nil {
		return nil, "", err
	}
	target := fmt.Sprintf("%s@%s/%s (from %s)",
		cfg.User, net.JoinHostPort(cfg.Host, cfg.Port), cfg.Database, source)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgwire.Dial(ctx, cfg)
	if err != nil {
		return nil, "", err
	}
	return conn, target, nil
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
