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
VALUES ($1, $2)
RETURNING id, email, name, created_at;
```

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
nested with `-- embed:`. Optional extras: `json:"column"` tags and a
`Querier` interface for test doubles.

## Commands

```
pgc generate [-c pgc.json] [-d url]  compile the queries into a Go package
pgc check    [-c pgc.json] [-d url]  compile without writing, for CI
pgc describe [-d url] "SELECT ..."   print parameter and column types
pgc version                          print the version
```

`pgc check` plus `git diff --exit-code` in CI catches queries that no longer
match the schema and generated code that drifted from its sources.

## Documentation

Full reference: **[DOC.md](DOC.md)** (Ukrainian: **[DOC.UK.md](DOC.UK.md)**) -
annotations, configuration, the type mapping, nullability rules and the
anatomy of the generated code.

## Contributing

Before submitting changes, run:

```shell
gofmt -l .
go vet ./...
go test ./...
```

## License

MIT - see [LICENSE](LICENSE).
