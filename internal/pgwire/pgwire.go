// Package pgwire is a minimal PostgreSQL frontend: enough of the version 3
// wire protocol to connect, authenticate and ask the server to describe a
// query. It exists so pgc can use a live development database as its type
// oracle - the server itself reports the exact parameter and column types of
// any statement - without linking a SQL parser or a driver.
//
// The package deliberately supports only what code generation needs:
//
//   - startup with cleartext, MD5 and SCRAM-SHA-256 authentication,
//     optionally over TLS;
//   - Parse/Describe/Sync to obtain a statement's parameter OIDs and result
//     columns (queries are never executed);
//   - the simple Query protocol in text format, used for catalog lookups
//     (pg_type, pg_attribute, pg_class).
//
// It is not a driver and is not meant to run application queries.
package pgwire
