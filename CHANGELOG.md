# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
