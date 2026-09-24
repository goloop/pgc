[![deps.dev](https://img.shields.io/badge/deps.dev-insights-4c8dbc)](https://deps.dev/go/github.com%2Fgoloop%2Fpgc) [![Go Reference](https://pkg.go.dev/badge/github.com/goloop/pgc.svg)](https://pkg.go.dev/github.com/goloop/pgc) [![License](https://img.shields.io/badge/license-MIT-brightgreen?style=flat)](https://github.com/goloop/pgc/blob/master/LICENSE) [![Stay with Ukraine](https://img.shields.io/static/v1?label=Stay%20with&message=Ukraine%20♥&color=ffD700&labelColor=0057B8&style=flat)](https://u24.gov.ua/)

# pgc

`pgc` compiles annotated SQL queries into a type-safe Go package for
PostgreSQL. You write plain SQL; pgc asks your development database what
every parameter and column really is - the server itself is the type oracle,
no SQL parsing involved - and generates code that reads like a person wrote
it.

The generated package imports only the standard library (`database/sql`,
`context`, `time`, ...). pgc itself has zero third-party dependencies: it
speaks the PostgreSQL wire protocol directly.

## Installation

```shell
go install github.com/goloop/pgc@latest
```

pgc is stable from v1: the commands and their exit codes, `pgc.json`, the
`pgc.lock.json` format, the migration history table, the connection URL and the
shape of the generated code do not change incompatibly within v1. The
"Compatibility" section of [DOC.md](DOC.md#compatibility) spells out what that
covers. Pin the version in CI (`@v1.0.1` rather than `@latest`).

Building from source requires Go 1.24 or newer. Prebuilt binaries for
Linux, macOS and Windows are attached to the
[releases](https://github.com/goloop/pgc/releases), with checksums.

Either way, generation needs a PostgreSQL database to ask (a disposable
local container is perfect).

## Quick start

Write queries next to your migrations:

```sql
-- queries/users.sql

-- name: GetUser :one
-- Returns a single user by primary key.
SELECT id, email, name, created_at
FROM users
WHERE id = $1;

-- name: CreateUser :one
INSERT INTO users (email, name)
VALUES (@email, @name)
RETURNING id, email, name, created_at;
```

Parameters are `$1..$N` or, when a statement has enough of them that counting
becomes the risk, `@name` - numbered for you, and named after what you wrote.

Point pgc at your development database and generate:

```shell
export PGC_DATABASE_URL="postgres://user:pass@localhost:5432/app?sslmode=disable"
pgc generate
```

Use the result like hand-written code, because it looks like hand-written
code:

```go
q := db.New(sqlDB) // *sql.DB

u, err := q.CreateUser(ctx, "ada@example.com", "Ada")
got, err := q.GetUser(ctx, u.ID)

// Inside a transaction - same methods:
tx, _ := sqlDB.BeginTx(ctx, nil)
err = q.WithTx(tx).DeleteUser(ctx, u.ID)
```

Every parameter and result is typed from the live schema: `bigint` becomes
`int64`, a nullable `text` becomes `*string`, `timestamptz` becomes
`time.Time`, a PostgreSQL enum becomes a named string type with constants,
`text[]` becomes `[]string` (including `WHERE id = ANY($1)`), and a domain
resolves to its base type. When the full column set of a table is selected,
the query returns the table's model struct; projections get their own row
struct; a single column comes back as a bare value; a joined table can be
nested with `-- embed:`. Expressions that cannot produce NULL - `count(...)`,
`coalesce(..., <literal>)` - come back as plain values rather than pointers.
Optional extras: `json:"column"` tags, a `Querier` interface for test doubles,
and `initialisms` for your domain's abbreviations, so `seo_title` becomes
`SEOTitle` instead of `SeoTitle`.

## Commands

```
pgc generate [-c pgc.json] [-d url]  compile the queries into a Go package
pgc check    [-c pgc.json] [-d url]  fail unless the package is up to date
pgc verify   [-c pgc.json] [-d url]  check the query types in pgc.lock.json
                                     against the database
pgc migrate  [up] [-c pgc.json] [-d url]
                                     apply pending migrations, in order
pgc migrate status                   list every migration's state
pgc migrate resolve <file> applied|retry
pgc migrate baseline <last-file>
pgc describe [-d url] "SELECT ..."   print parameter and column types
pgc version                          print the version
```

`pgc migrate` applies plain-SQL migration files exactly once each, in name
order - one transaction per file, an advisory lock against concurrent runs
and a history table checked before anything runs, so the whole database
lifecycle lives in one tool: `pgc migrate`, `pgc generate`, `pgc check`. An
applied file that was edited or deleted stops the run, a file with its own
`COMMIT` is refused, and a no-transaction file that fails half way is never
silently repeated. In CI, `pgc check` fails when the generated package no
longer matches its queries, and `pgc migrate status` when the migration
history needs attention.

**Generating needs a database only once.** `pgc generate` records what the
server said in `pgc.lock.json`; commit it, and every later run without a
database URL generates the identical package from that record - a fresh clone,
a CI job, a container build. The record is refused the moment a query stops
matching it, and `pgc verify` reports drift in the one job that does have a
database.

## Documentation

New to pgc? The step-by-step **[TUTORIAL.md](TUTORIAL.md)** (Ukrainian:
**[TUTORIAL.UK.md](TUTORIAL.UK.md)**) takes you from an empty directory to
a running program in about twenty minutes - Docker database, migrations,
annotations, generation and usage, all copy-pasteable.

Full reference: **[DOC.md](DOC.md)** (Ukrainian: **[DOC.UK.md](DOC.UK.md)**) -
migrations, annotations, configuration, the type mapping, nullability rules,
the anatomy of the generated code and the compatibility promise.

Changes between releases: **[CHANGELOG.md](CHANGELOG.md)**.

## Contributing

Before submitting changes, run:

```shell
gofmt -l .
go vet ./...
go test ./...
```

The integration tests run against a real PostgreSQL. They create and drop
databases of their own, so point them at a disposable server only:

```shell
docker run -d --rm --name pgc-test -e POSTGRES_PASSWORD=test \
  -p 127.0.0.1:55432:5432 postgres:17-alpine
PGC_DATABASE_URL="postgres://postgres:test@127.0.0.1:55432/postgres?sslmode=disable" \
  go test -tags integration ./...
```

## License

MIT - see [LICENSE](LICENSE).
