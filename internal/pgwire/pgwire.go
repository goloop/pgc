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
//   - the simple Query protocol in text format: Query for catalog lookups
//     (pg_type, pg_attribute, pg_class) and the migration history, Exec for
//     migration scripts, whose rows are read and dropped;
//   - CancelRequest, to stop a statement from another goroutine.
//
// A connection is used from one goroutine at a time (Cancel excepted). After a
// timeout, a transport error or a message out of protocol it is closed rather
// than reused, since part of an answer may still be on its way; a server error
// leaves it usable. COPY is not supported: COPY FROM STDIN fails at once, and
// COPY TO STDOUT output is discarded.
//
// It is not a driver and is not meant to run application queries.
package pgwire
