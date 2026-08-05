# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.7.3] - 2026-08-05

### Fixed
- `make check` no longer depends on the version guard added in 0.7.2. Between
  releases the constant is meant to be ahead of the newest tag, so the everyday
  command failed precisely when the work had been done in the right order. The
  guard stays on `dist` and `release`, which is where it belongs.
- The outer-join warning stays quiet once a query states the nullability of
  its result columns. It fired on every outer join, including the ones written
  correctly, so a project that had done the work still saw the warning on every
  run - and learned to scroll past it, which is exactly how the queries that do
  need it get missed.

## [0.7.2] - 2026-08-05

### Fixed
- `pgc version` prints the real version again, and `pgc.lock.json` records it:
  the constant had been left at `0.7.0` through the 0.7.1 release, so an
  installed 0.7.1 reported itself as 0.7.0 and stamped that into every
  snapshot it wrote.
- `make dist` and `make release` now refuse to run while the constant and the
  newest tag disagree. The constant has fallen behind twice; a check is worth
  more than remembering.

## [0.7.1] - 2026-08-05

### Documentation
- The README covers what 0.7.0 added: `@name` parameters in the quick-start
  query, the `initialisms` configuration, and the expressions that no longer
  come back as pointers.

## [0.7.0] - 2026-08-05

### Added
- **Generation no longer needs a database every time.** `pgc generate` records
  what the server said about every query in `pgc.lock.json`, beside the
  configuration; commit it, and a run with no database URL generates the
  identical package from that record. A fresh clone, a CI job and a container
  build stop needing a PostgreSQL of their own, and the file is the visible
  contract between the migrations, the `.sql` files and the generated Go.

  The record is only used while it is still true: every query carries a
  fingerprint of its SQL, and one that has changed - or that the record has
  never seen - is refused rather than generated from types read for a
  different statement. The migration files are fingerprinted too; a change
  there is a warning, since nothing offline can tell whether it matters.
- `pgc verify` re-describes everything against a database and reports how
  `pgc.lock.json` differs, exiting non-zero on any drift. It is the other half
  of generating without one: run it in the CI job that has a database, and let
  every other job read the record. It compares the recorded catalog as well as
  the queries, so `ALTER TYPE ... ADD VALUE` is reported: every statement still
  describes identically after it, while the generated constants, the values
  list and the parser all change.
- **Named parameters.** A query may write `@name` instead of `$N`; the names
  are numbered by first appearance before the statement reaches the server,
  and the generated argument takes the name written. A repeated `@name` is one
  parameter. Adding a column to a 26-parameter `INSERT` was an edit plus
  renumbering everything after it, and getting that wrong compiled cleanly and
  put values in the wrong columns. `-- override:` accepts `@name` as well. The
  two styles do not mix inside one query, and a query using names has no use
  for `-- param:`; both are errors.
- **Project initialisms.** `"initialisms": ["seo", "cdn"]` in `pgc.json` adds
  to the built-in list, so `seo_title` becomes `SEOTitle` rather than
  `SeoTitle`. The list only adds: a built-in cannot be removed, so the same
  schema gives the same names in every project.

### Changed
- `ai` is a built-in initialism. A column named `ai_generated` now generates
  `AIGenerated` where it was `AiGenerated`; rename the field at the call sites
  that use it.
- **A few expressions are no longer rendered as pointers.** `count(...)`,
  `coalesce(..., <literal>)` and non-null literals with a cast to a built-in
  type cannot produce NULL, so they come back as `int64` rather than `*int64`
  and the `-- override: <name> notnull` they used to need can go. Everything
  else - `max(...)`, `sum(...)`, `coalesce(a, b)` of two columns, arithmetic,
  scalar subqueries - stays nullable, and the inference gives up for the whole
  query when the output list cannot be lined up with the columns with
  certainty (a `*`, a `UNION`, a `WITH`, a `DISTINCT ON`). A rule that is right
  most of the time would be worse than none.

## [0.6.1] - 2026-08-05

### Fixed
- The generated `querier.go` no longer imports `encoding/json`, `time` or any
  other type-driven import for a query whose parameters collapse into an
  `XxxParams` struct. Past four parameters the interface signature spells out
  the struct name, not the individual types, so collecting those types for the
  import list produced an import the file never used and a generated package
  that did not compile.
- The four-argument threshold is written down once, as `gen.Query`'s
  `UsesParamStruct` method, and everything that depends on it reads it there:
  the method signature and the struct definition, the `Querier` interface and
  its import list, and the check that reserves the name `XxxParams` at package
  level. Those three sites each carried their own copy of the rule, which is
  how the import list came to disagree with the signature in the first place;
  a fourth copy decided whether a genuine name collision was reported at all.
- `pgc version` prints the real version again: the constant had been left at
  `0.5.1` through the 0.6.0 release.

## [0.6.0] - 2026-07-12

### Security
- The wire protocol client no longer trusts message field counts blindly: a
  `DataRow`, `ParameterDescription` or `RowDescription` with a negative count
  (for example `0xffff` read as `int16`) returns an error instead of panicking.
- A truncated authentication (`R`) message is rejected instead of being read as
  `AuthenticationOk` (its zeroed code).
- The SCRAM iteration count is bounded (ceiling `1<<24`; real servers use
  ~4096), so a hostile server cannot force unbounded PBKDF2 work, and a server
  nonce that merely equals the client nonce (no server suffix) is rejected.
- `migrate` pins `search_path` to `public` and fully qualifies the
  `pgc_migrations` bookkeeping table, so a decoy table in another schema cannot
  shadow the real migration history and hide pending migrations.

### Fixed
- Generated identifiers are always valid, compilable Go: `CamelCase` now
  capitalizes on a rune boundary (a non-ASCII column name is no longer mangled),
  drops non-identifier runes, and prefixes a leading digit or an empty result
  with `X`. A quoted PostgreSQL name that is not a valid Go identifier no longer
  produces code that fails to compile.
- The migration statement splitter understands `E'...'` escape strings, so a
  backslash-escaped quote with an embedded semicolon no longer splits a
  statement and breaks a no-transaction migration.
- Parameter-name inference reads a quoted identifier with a doubled quote whole
  (`"user""id"` is `user"id`), instead of truncating at the first inner quote.
- Two custom types from different packages that share a base name now get
  distinct selectors and explicit import aliases, and an import path element
  with a dash yields a valid selector, so the generated import block compiles.

## [0.5.1] - 2026-07-11

### Fixed
- With `json_tags` enabled, an embedded struct's json tag now follows the Go
  field name (its `as` alias when given) instead of the source table name. A
  query with `-- embed: users as Author` now tags the field `json:"author"`
  rather than `json:"users"`, so the tag matches the field a caller sees.
  Regular columns are unchanged.

## [0.5.0] - 2026-07-11

### Added
- `sslrootcert` in the connection URL: verify the server's certificate
  chain against a provider-issued CA file instead of the system roots -
  the usual arrangement for managed production databases. New `verify-ca`
  mode checks the chain without the host name; `require` with an
  `sslrootcert` is promoted to `verify-ca`, matching the standard
  connection-parameter behavior.

### Fixed
- A TLS negotiation failure now surfaces as a clean error instead of a
  panic.

## [0.4.1] - 2026-07-11

### Fixed
- `pgc migrate` no longer applies the 30-second protocol deadline to
  migration statements: an index build or a backfill now waits for as long
  as the server needs instead of killing the connection mid-DDL. Dead
  peers are still caught by TCP keepalives.

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
