# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.4.0] - 2026-07-11

### Added
- Every enum gets three companions - `Valid()`, `<Enum>Values()` and
  `Parse<Enum>(string)` - so application-side validation regenerates
  together with the schema and can never go stale after
  `ALTER TYPE ... ADD VALUE`.

### Changed
- Model reuse and `-- embed:` now match the table's full column **set** in
  any order, not only in physical attnum order. After
  `ALTER TABLE ADD COLUMN` the attnum order stops matching what a human
  writes; Scan arguments follow the SELECT order, mapped onto the right
  struct fields, so correctness is unchanged. The embed mismatch error now
  explains the rule instead of surfacing as a duplicate-field complaint.

## [0.3.1] - 2026-07-11

### Fixed
- `querier.go` no longer imports packages that only row struct fields use
  (an unused `"time"` import broke the build when a row carried a
  timestamp); the interface mentions structs by name alone.

## [0.3.0] - 2026-07-11

### Added
- `pgc migrate` applies the plain-SQL files of the migrations directory
  exactly once each, in name order, over pgc's own wire client - no
  external migration tool needed. Each file runs in its own transaction
  together with its bookkeeping row in `pgc_migrations`; a
  `-- pgc: no-transaction` first line opts a file out for statements
  PostgreSQL refuses inside transactions. File hashes are recorded and
  editing an applied file warns forever; out-of-order files apply with a
  warning; the whole run holds an advisory lock so concurrent runs queue.
  Forward-only by design - no down migrations.
- `pgc migrate status` lists applied and pending files.
- The `migrations` key in pgc.json names the directory (default
  `migrations`).

## [0.2.1] - 2026-07-11

### Added
- A step-by-step tutorial (`TUTORIAL.md`, Ukrainian `TUTORIAL.UK.md`):
  Docker database with compose, migrations, the full annotation language,
  generation, usage, the change loop, CI and troubleshooting - every
  command verified end to end on a clean machine.

### Fixed
- `$1 = ANY(col)` and `$1 = ALL(col)` now name the parameter after the
  column; `ANY`/`ALL`/`SOME`/`EXISTS`/`DISTINCT` can no longer be picked
  up as parameter names.

## [0.2.0] - 2026-07-11

### Added
- One-dimensional arrays of the basic types map to plain Go slices
  (`bigint[]` to `[]int64`), scanned and sent through generated adapters in
  `pgarray.go` that speak the array text format - no driver-specific array
  support needed. A nil slice is NULL; NULL elements and multidimensional
  arrays are rejected with a clear error.
- `-- embed: <table> [as <Field>]` nests a joined table's full row as its
  model struct inside the query's row struct.
- Go types from other modules in overrides and the `types` map, written
  with their full import path (`github.com/google/uuid.UUID`).
- `"json_tags": true` adds `json:"column_name"` tags to model and row
  structs.
- `"interface": true` emits `querier.go` with a `Querier` interface that
  `*Queries` satisfies.
- The `rename` map accepts schema-qualified table names
  (`"audit.users": "AuditUser"`).
- Parameter names are inferred from `= ANY($1)` / `ALL($1)` comparisons.

### Fixed
- Domains now resolve to their base types instead of failing as
  unsupported.
- Two same-named tables from different schemas (or any other generated name
  collision) are reported as an error instead of emitting invalid Go.

## [0.1.0] - 2026-07-10

Initial release.

### Added
- `pgc generate` and `pgc check`: compile annotated SQL queries into a
  type-safe Go package, using a live development database as the type
  oracle - statements are prepared and described over the wire protocol,
  never executed and never parsed by pgc itself.
- Query annotations: `-- name: <Name> :one|:many|:iter|:exec|:execrows`,
  doc comments that become godoc, `-- override:` for per-column and
  per-parameter types and nullability, `-- param:` for explicit argument
  names.
- Parameter names inferred from the SQL (`id = $1`, INSERT column lists,
  `LIMIT`/`OFFSET`); `argN` as the fallback.
- Result shapes: a full table row returns the table's model struct, a
  projection returns a per-query row struct, a single column returns a bare
  value; up to three arguments stay positional, four or more become a
  params struct.
- `:iter` streams rows as an `iter.Seq2[T, error]` sequence.
- PostgreSQL enums become named string types with one constant per label.
- Nullability from the catalog (`pointer` or `sqlnull` rendering);
  expressions default to nullable; outer joins produce a warning asking
  for explicit overrides.
- `pgc describe`: print the parameter and column types of any statement.
- `pgc.json` configuration: queries dir, output dir and package, nullable
  mode, per-type Go replacements, table renames. The connection URL comes
  only from `PGC_DATABASE_URL`/`DATABASE_URL`/`-d`.
- Zero third-party dependencies in the tool and in the generated code:
  pgc speaks the wire protocol (SCRAM-SHA-256, MD5 and cleartext
  authentication, optional TLS) and the generated package imports only the
  standard library.
