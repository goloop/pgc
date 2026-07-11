# pgc tutorial - from zero to typed queries

This is a complete walkthrough: you will start PostgreSQL in Docker, write a
migration and a few annotated SQL queries, generate a typed Go package with
pgc and use it from a real program. Every command is meant to be copied and
run as-is; nothing is assumed beyond the prerequisites. Budget about twenty
minutes.

Ukrainian version: **[TUTORIAL.UK.md](TUTORIAL.UK.md)**.
Reference documentation: **[DOC.md](DOC.md)**.

## Contents

- [0. Prerequisites](#0-prerequisites)
- [1. Create the project](#1-create-the-project)
- [2. Start PostgreSQL in Docker](#2-start-postgresql-in-docker)
- [3. Write and apply a migration](#3-write-and-apply-a-migration)
- [4. Install pgc](#4-install-pgc)
- [5. Point pgc at the database](#5-point-pgc-at-the-database)
- [6. Ask the server about a query](#6-ask-the-server-about-a-query)
- [7. Write the queries file](#7-write-the-queries-file)
- [8. The annotation language](#8-the-annotation-language)
- [9. Configure generation: pgc.json](#9-configure-generation-pgcjson)
- [10. Generate](#10-generate)
- [11. Use the generated code](#11-use-the-generated-code)
- [12. The change loop](#12-the-change-loop)
- [13. Keep it honest in CI](#13-keep-it-honest-in-ci)
- [14. Troubleshooting](#14-troubleshooting)

## 0. Prerequisites

Two tools, both checkable in one line each:

```sh
go version        # want go1.24 or newer
docker compose version
```

If `go` is missing, install it from <https://go.dev/dl/>. If
`docker compose` is missing, install Docker Desktop or the docker-ce +
compose-plugin packages for your system. You do not need PostgreSQL
installed on the machine - it will live in a container.

## 1. Create the project

```sh
mkdir notes && cd notes
go mod init example.com/notes
```

`go mod init` declares a Go module. The path `example.com/notes` is only a
name; if you plan to publish the project, use its real repository path
(`github.com/you/notes`) instead.

## 2. Start PostgreSQL in Docker

Create `docker-compose.yaml` in the project root:

```yaml
services:
  db:
    image: postgres:17-alpine
    environment:
      POSTGRES_USER: app
      POSTGRES_PASSWORD: secret
      POSTGRES_DB: app
    ports:
      - "127.0.0.1:5433:5432"
    volumes:
      - ./migrations:/migrations:ro
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U app -d app"]
      interval: 1s
      retries: 30
```

Line by line:

- **image** - the official PostgreSQL 17 image, Alpine variant (small).
- **environment** - first-boot settings: a user `app` with password
  `secret` and a database `app` are created automatically.
- **ports** - the mapping `"127.0.0.1:5433:5432"` reads
  *host-address:host-port:container-port*: the server listens on 5432
  inside the container and is reachable from your machine as
  `127.0.0.1:5433`. Port 5433 is chosen so it never clashes with a
  PostgreSQL you might already run locally on 5432; binding to `127.0.0.1`
  keeps it invisible to the network.
- **volumes** - your `./migrations` directory appears inside the container
  as `/migrations`, read-only, so `psql` in the container can execute the
  files.
- **healthcheck** - lets `docker compose` know when the server is actually
  ready, not merely started.

Create the migrations directory **before** starting the container - Docker
creates missing bind-mount sources as root-owned directories, and then step
3 would fail with a permission error:

```sh
mkdir migrations
docker compose up -d
docker compose ps
```

Repeat `docker compose ps` until the `db` service shows `(healthy)` - on
the first run the image download takes the longest.

## 3. Write and apply a migration

A migration is a plain SQL file that moves the schema one step forward.
Create `migrations/001_init.sql`:

```sql
CREATE TYPE note_status AS ENUM ('draft', 'published');

CREATE TABLE notes (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    title      text NOT NULL,
    body       text NOT NULL DEFAULT '',
    status     note_status NOT NULL DEFAULT 'draft',
    tags       text[] NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);
```

What the column types buy you later: `GENERATED ALWAYS AS IDENTITY` is the
modern auto-increment, `note_status` is a real enum (it will become a Go
type with constants), `text[]` is an array (it will become `[]string`), and
`timestamptz` becomes `time.Time`.

Apply every migration file, in name order, with the `psql` that ships
inside the container:

```sh
for f in migrations/*.sql; do
  docker compose exec -T db psql -U app -d app -v ON_ERROR_STOP=1 -f "/$f"
done
```

Reading that command: `exec -T db` runs a program inside the `db` service
without allocating a terminal; `-U app -d app` selects the user and
database; `-v ON_ERROR_STOP=1` makes `psql` fail loudly on the first error
instead of continuing; `-f "/$f"` executes the file - the leading `/` works
because `./migrations` is mounted at `/migrations`. Numbered file names
(`001_...`, `002_...`) keep the order stable as the project grows; any
dedicated migration tool works just as well - pgc does not care how the
schema got there.

Verify:

```sh
docker compose exec db psql -U app -d app -c '\dt'
```

You should see the `notes` table.

## 4. Install pgc

```sh
go install github.com/goloop/pgc@latest
pgc version
```

`go install` puts the binary into `$(go env GOPATH)/bin` - usually
`~/go/bin`. If `pgc version` says "command not found", add that directory
to your PATH:

```sh
export PATH="$PATH:$(go env GOPATH)/bin"
```

Two alternatives:

- **Prebuilt binaries** for Linux, macOS and Windows are attached to the
  [releases](https://github.com/goloop/pgc/releases), with checksums -
  download, unpack, put `pgc` on your PATH.
- **From a source checkout**: `make build` produces `./pgc`.

If a release was tagged minutes ago, the Go module proxy may not have seen
it yet; `GOPROXY=direct go install github.com/goloop/pgc@latest` fetches
straight from the repository.

## 5. Point pgc at the database

pgc reads the connection URL from the environment - never from a config
file, so credentials stay out of the repository:

```sh
export PGC_DATABASE_URL="postgres://app:secret@127.0.0.1:5433/app?sslmode=disable"
```

The URL anatomy, matching the compose file exactly:

```
postgres://  app  :  secret  @  127.0.0.1 : 5433  /  app  ?sslmode=disable
scheme       user    password   host        port     db      options
```

`sslmode=disable` is right for a local container - there is no TLS to
speak. For remote servers use `require` or `verify-full`.

## 6. Ask the server about a query

Before generating anything, see the core idea in action. pgc does not parse
SQL - it asks the running server to *describe* a statement (prepare without
executing) and the server reports every type:

```sh
pgc describe "SELECT id, title, tags FROM notes WHERE id = $1"
```

```
server 17.10

Parameters:
  $1   int8

Columns:
  id                   int8         not null  notes.id
  title                text         not null  notes.title
  tags                 _text        not null  notes.tags
```

The server said it, so the generated code will match it. A broken query is
rejected here with the exact error position - nothing wrong ever reaches
your Go code.

## 7. Write the queries file

Create `queries/notes.sql`. Each query is plain SQL with a small annotation
header; the comment text under `-- name:` becomes the godoc of the
generated method:

```sql
-- name: CreateNote :one
-- Inserts a note and returns it.
INSERT INTO notes (title, body, tags)
VALUES ($1, $2, $3)
RETURNING id, title, body, status, tags, created_at;

-- name: GetNote :one
-- Returns one note by primary key.
SELECT id, title, body, status, tags, created_at
FROM notes
WHERE id = $1;

-- name: ListNotes :many
-- Returns notes, newest first.
SELECT id, title, body, status, tags, created_at
FROM notes
ORDER BY created_at DESC, id DESC
LIMIT $1 OFFSET $2;

-- name: SearchByTag :many
-- Returns every note carrying the tag.
-- param: $1 tag
SELECT id, title, body, status, tags, created_at
FROM notes
WHERE $1 = ANY(tags)
ORDER BY id;

-- name: PublishNote :execrows
-- Marks a note as published and reports how many rows changed.
UPDATE notes
SET status = 'published'
WHERE id = $1;

-- name: CountNotes :one
-- override: total int64 notnull
SELECT count(*) AS total
FROM notes;

-- name: IterNotes :iter
-- Streams every note, oldest first.
SELECT id, title, body, status, tags, created_at
FROM notes
ORDER BY id;

-- name: DeleteNote :exec
DELETE FROM notes
WHERE id = $1;
```

Two annotations beyond `-- name:` appear above and deserve a word now (the
full language is in the next section):

- `-- param: $1 tag` names the first argument `tag`. pgc infers argument
  names from the SQL (`id = $1` names it `id`, `LIMIT $1` names it
  `limit`), and falls back to `argN` when the SQL gives no hint - the
  annotation is the explicit escape hatch.
- `-- override: total int64 notnull` adjusts one result column: `count(*)`
  is an expression, the catalog cannot promise it is never NULL, so pgc
  would make it a pointer; you know better and say so.

## 8. The annotation language

The complete set of annotations a query can carry, all of them comment
lines between the `-- name:` header and the SQL body.

### -- name: <Name> :command

Starts a query. `<Name>` must be an exported Go identifier, unique across
the package. The command decides the method's shape:

| Command | Generated signature | Use for |
|---|---|---|
| `:one` | `(T, error)`; `sql.ErrNoRows` when nothing matches | fetch by key, INSERT ... RETURNING |
| `:many` | `([]T, error)` | lists |
| `:iter` | `iter.Seq2[T, error]`; stops at the first error | large results, streaming |
| `:exec` | `error` | fire-and-forget statements |
| `:execrows` | `(int64, error)` - affected row count | UPDATE/DELETE where the count matters |

The `T` above follows three rules: selecting a table's **full column set**
returns the table's model struct (`Note`); a **projection** returns a
per-query struct (`SearchNotesRow`); a **single column** returns a bare
value (`count(*)` gives `int64`).

### Doc comments

Every `--` line that is not one of the annotations below becomes the godoc
sentence of the generated method. Write it like a godoc: third person,
starting with a verb ("Returns...", "Inserts...") - pgc prefixes the method
name automatically.

### -- param: $N <name>

Names one `$N` argument when the SQL offers no usable hint. Inference
covers the common shapes - `col = $1` (either side), `col IN ($1)`,
`col = ANY($1)` and `$1 = ANY(col)`, `INSERT ... VALUES` by position,
`LIMIT`/`OFFSET` - and everything else becomes `arg1`, `arg2`, ... until
you name it.

### -- override: <column|$N> [go-type] [notnull|nullable]

Adjusts one result column or one parameter when the catalog cannot know
better:

```sql
-- override: total int64 notnull        -- expression: force NOT NULL
-- override: avatar_url nullable        -- flip only the nullability
-- override: $3 *time.Time              -- parameter may be NULL
-- override: $1 github.com/google/uuid.UUID   -- type from another module
```

An explicit Go type is taken verbatim, its nullability included. A type
from another module is written with its full import path; the import is
added to the generated file, and the type must implement
`sql.Scanner`/`driver.Valuer` as usual. Naming a column or parameter that
the statement does not have is an error - typos never pass silently.

### -- embed: <table> [as <Field>]

When a query joins in a whole row of another table, nests it as that
table's model instead of flattening:

```sql
-- name: NotesWithAuthor :many
-- embed: authors as Author
SELECT n.id, n.title, a.id, a.name, a.email
FROM notes n
JOIN authors a ON a.id = n.author_id;
```

```go
type NotesWithAuthorRow struct {
	ID     int64
	Title  string
	Author Author
}
```

The annotation matches the first contiguous run of result columns that is
exactly the table's full column list, in order - select the table's columns
together (`a.*` does that). Repeat the annotation to embed several tables;
use `as` to name the field, mandatory when embedding the same table twice.

## 9. Configure generation: pgc.json

Create `pgc.json` in the project root:

```json
{
  "queries": "queries",
  "out": "internal/db",
  "package": "db",
  "json_tags": true,
  "rename": {
    "notes": "Note"
  }
}
```

Every key, including the ones this tutorial leaves at their defaults:

| Key | Default | Meaning |
|---|---|---|
| `queries` | `queries` | directory with the `.sql` files |
| `out` | `internal/db` | where the generated package is written |
| `package` | base of `out` | generated package name |
| `nullable` | `pointer` | nullable columns as `*T`; `sqlnull` gives `sql.Null[T]` |
| `json_tags` | `false` | add `` `json:"column_name"` `` tags to structs |
| `interface` | `false` | also emit a `Querier` interface for test doubles |
| `types` | `{}` | per-PostgreSQL-type Go replacements, e.g. `{"uuid": "string"}` |
| `rename` | `{}` | table to struct name, e.g. `{"notes": "Note"}`; schema-qualified keys allowed |

The `rename` entry matters more than it looks: pgc never guesses singular
forms, so without it the `notes` table becomes a struct named `Notes`.

## 10. Generate

```sh
pgc generate
```

```
internal/db/db.go
internal/db/models.go
internal/db/pgarray.go
internal/db/notes.sql.go
```

The tour:

- **db.go** - the plumbing: a `DBTX` interface (satisfied by both `*sql.DB`
  and `*sql.Tx`), the `Queries` struct, `New` and `WithTx`.
- **models.go** - the `NoteStatus` enum with its constants and the `Note`
  struct, typed from the live schema (`Tags []string`, `CreatedAt
  time.Time`, json tags in place).
- **pgarray.go** - appears because the schema uses arrays: small adapters
  that translate PostgreSQL's array text format; you never call them
  directly.
- **notes.sql.go** - one method per query, with your doc comments and
  explicit `Scan` calls. Open it - it is meant to be read.

One thing this schema does not show: a **nullable** column. Every column
here is `NOT NULL`, so every field is a plain value; a nullable `text`
column would come out as `*string` (or `sql.Null[string]` with
`"nullable": "sqlnull"`). The full mapping table is in
[DOC.md](DOC.md#type-mapping).

Re-run `pgc generate` any time the schema or the queries change; the files
are overwritten deterministically, so `git diff` shows exactly what the
change did.

## 11. Use the generated code

The generated package speaks plain `database/sql`, so the program needs a
driver for it - any PostgreSQL driver registered with `database/sql`
works the same way. This tutorial uses one of the common ones:

```sh
go get github.com/lib/pq
```

Create `main.go`:

```go
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"

	"example.com/notes/internal/db"

	_ "github.com/lib/pq"
)

func main() {
	ctx := context.Background()

	sqlDB, err := sql.Open("postgres", os.Getenv("PGC_DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer sqlDB.Close()

	q := db.New(sqlDB)

	// Create two notes; the enum default makes them drafts.
	first, err := q.CreateNote(ctx, "Hello pgc", "The very first note.",
		[]string{"intro", "pgc"})
	if err != nil {
		log.Fatal(err)
	}
	if _, err := q.CreateNote(ctx, "Second", "", []string{"pgc"}); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("created #%d %q status=%s tags=%v\n",
		first.ID, first.Title, first.Status, first.Tags)

	// Publish the first one; :execrows reports the affected count.
	changed, err := q.PublishNote(ctx, first.ID)
	if err != nil || changed != 1 {
		log.Fatal("publish: ", err, changed)
	}

	// Arrays work in queries too: $1 = ANY(tags).
	tagged, err := q.SearchByTag(ctx, "intro")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("tagged %q: %d note(s)\n", "intro", len(tagged))

	// A scalar comes back as a bare value.
	total, err := q.CountNotes(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("total notes:", total)

	// Stream without materializing the whole list.
	for note, err := range q.IterNotes(ctx) {
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("  #%d %s [%s]\n", note.ID, note.Title, note.Status)
	}

	// The same Queries value works inside a transaction.
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		log.Fatal(err)
	}
	if err := q.WithTx(tx).DeleteNote(ctx, first.ID); err != nil {
		log.Fatal(err)
	}
	if err := tx.Rollback(); err != nil { // change of heart
		log.Fatal(err)
	}
	if _, err := q.GetNote(ctx, first.ID); err != nil {
		log.Fatal("the rollback should have kept the note: ", err)
	}
	fmt.Println("rollback kept the note, as a rollback should")

	// And sql.ErrNoRows is exactly what the docs promise.
	if _, err := q.GetNote(ctx, 99999); errors.Is(err, sql.ErrNoRows) {
		fmt.Println("missing note reports sql.ErrNoRows")
	}
}
```

Run it:

```sh
go run .
```

```
created #1 "Hello pgc" status=draft tags=[intro pgc]
tagged "intro": 1 note(s)
total notes: 2
  #1 Hello pgc [published]
  #2 Second [draft]
rollback kept the note, as a rollback should
missing note reports sql.ErrNoRows
```

## 12. The change loop

The final project layout, for orientation:

```
notes/
├── docker-compose.yaml
├── migrations/001_init.sql
├── queries/notes.sql
├── pgc.json
├── internal/db/          <- generated, never edited by hand
├── main.go
├── go.mod
└── go.sum
```

Who is the source of truth for what: **migrations** define the database;
the **live database** defines the types pgc generates; **queries/*.sql**
define the API surface; `internal/db/` is always a derived artifact. When
they disagree, the loop below re-aligns them.

Changing a table:

```sh
$EDITOR migrations/002_add_author.sql   # 1. a new migration file
for f in migrations/*.sql; do ... done  # 2. apply (the loop from step 3)
pgc generate                            # 3. regenerate
go build ./...                          # 4. the compiler shows every impact
```

Changing or adding a query is the same loop without the first two steps.
The compile errors in step 4 are the feature, not the problem: every call
site that no longer matches the schema is pointed out before anything runs.

## 13. Keep it honest in CI

Two commands keep the generated code and the schema from drifting apart:

```sh
pgc check              # compiles every query against the DB, writes nothing
pgc generate && git diff --exit-code   # fails if committed code is stale
```

In CI, start a disposable PostgreSQL service, apply the migrations the same
way as in step 3, and pin versions - both pgc's and Go's:

```sh
go install github.com/goloop/pgc@v0.2.1
pgc generate
git diff --exit-code
```

## 14. Troubleshooting

How to read a pgc error first: compile-time problems come as
`queries/notes.sql:12: GetNote: <what and where>` - the file, the line of
the `-- name:` header and the query name point at the SQL to fix; a server
complaint additionally carries the character position inside the statement
(`position 8`). Runtime problems come from `database/sql` as usual.

| Symptom | Cause | Fix |
|---|---|---|
| `connection refused` | the container is not up, or the port is wrong | `docker compose ps`; the URL port must match the left side of the `ports:` mapping |
| `password authentication failed` | URL credentials differ from the compose environment | compare `POSTGRES_USER`/`POSTGRES_PASSWORD` with the URL |
| `relation "notes" does not exist` | migrations were not applied to this database | re-run step 3; check `-d app` matches the URL database |
| `no database url` | `PGC_DATABASE_URL` is not exported in this shell | re-run the `export` from step 5 |
| `unsupported PostgreSQL type "xxx"` | a column type pgc has no default mapping for | add `"types": {"xxx": "string"}` in pgc.json, or an override on the column |
| `a result column has no name` | an expression without an alias | give it one: `count(*) AS total` |
| `generated name X collides` | two tables map to the same struct name | add a `rename` entry, schema-qualified if needed |
| `embed ...: no remaining run` | the embedded table's columns are not contiguous or not complete | select them together and in full: `a.*` |
| `go install` cannot find a fresh version | the module proxy has not indexed the tag yet | `GOPROXY=direct go install github.com/goloop/pgc@latest` |
| `pgc: command not found` after go install | `~/go/bin` is not on PATH | `export PATH="$PATH:$(go env GOPATH)/bin"` |

When you outgrow the tutorial, the full reference is **[DOC.md](DOC.md)**:
every configuration key, the complete type mapping, nullability rules,
enums, arrays, embedding and the CI recipe in more detail.
