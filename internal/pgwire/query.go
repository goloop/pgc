package pgwire

import "fmt"

// maxResult caps the total text a Query may hold in memory. maxMessage bounds
// one row; this bounds them all, so a statement that returns more than pgc's
// catalog lookups ever need fails instead of filling the heap.
const maxResult = 64 << 20 // 64 MiB

// Query runs SQL over the simple-query protocol and returns every row as
// text-format values. A NULL becomes ("", false); everything else is
// (value, true).
//
// This is for pgc's own lookups (pg_type, pg_attribute, pg_class and the
// migration history) - small, trusted queries built by pgc itself. It is not
// meant for application data: past maxResult bytes it drains the rest of the
// answer, keeping the connection usable, and reports an error. Scripts whose
// rows nobody reads belong to Exec.
func (c *Conn) Query(sql string) ([][]Value, error) {
	var rows [][]Value
	err := c.simpleQuery(sql, &rows)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// Exec runs SQL - one statement or a whole script - over the simple-query
// protocol and discards any rows it returns, so a migration's backfill with a
// large RETURNING costs no memory. It returns the first error the server
// reported.
func (c *Conn) Exec(sql string) error {
	return c.simpleQuery(sql, nil)
}

// simpleQuery sends one Query message and reads the answer through the
// closing ReadyForQuery. Rows are collected into *rows when rows is non-nil.
//
// Only a server error leaves the connection usable, because only then has the
// whole answer been read. A transport error, a timeout or a message pgc does
// not understand leaves an unknown part of it on the wire, and the connection
// is closed rather than let the next request read this one's answer.
func (c *Conn) simpleQuery(sql string, rows *[][]Value) error {
	if err := c.usable(); err != nil {
		return err
	}
	c.begin()
	defer c.done()

	var q writeBuf
	q.cstring(sql)
	if err := c.sendFlush('Q', q); err != nil {
		return c.fail(err)
	}

	var (
		srvErr   *ServerError
		tooLarge bool
		held     int
	)
	for {
		typ, payload, err := c.recv()
		if err != nil {
			return c.fail(err)
		}
		switch typ {
		case 'T': // RowDescription - column meta not needed here
		case 'D': // DataRow
			if rows == nil || tooLarge {
				continue
			}
			row, size, err := parseDataRow(payload)
			if err != nil {
				return c.fail(err)
			}
			if held += size; held > maxResult {
				tooLarge = true
				*rows = nil
				continue
			}
			*rows = append(*rows, row)
		case 'C': // CommandComplete
		case 'I': // EmptyQueryResponse
		case 'E':
			if srvErr == nil {
				srvErr = parseServerError(payload)
			}
		case 'G': // CopyInResponse: the server now waits for data pgc has not got
			var b writeBuf
			b.cstring("pgc does not supply COPY FROM STDIN data; " +
				"load it with INSERT, or from a server-side file")
			if err := c.sendFlush('f', b); err != nil { // CopyFail
				return c.fail(err)
			}
		case 'H': // CopyOutResponse: the data that follows is discarded
		case 'd', 'c': // CopyData, CopyDone of a COPY TO STDOUT
		case 'W': // CopyBothResponse belongs to replication, never asked for here
			return c.fail(fmt.Errorf("pgwire: unexpected COPY BOTH response"))
		case 'N', 'A': // NoticeResponse, NotificationResponse
		case 'S':
			c.parameterStatus(payload)
		case 'Z': // ReadyForQuery
			if err := c.ready(payload); err != nil {
				return err
			}
			if srvErr != nil {
				return srvErr
			}
			if tooLarge {
				return fmt.Errorf(
					"pgwire: the result is larger than %d MiB; pgc reads "+
						"results only for its own small lookups", maxResult>>20)
			}
			return nil
		default:
			return c.fail(fmt.Errorf(
				"pgwire: unexpected message %q in a query response", typ))
		}
	}
}

// parseDataRow decodes a DataRow payload and reports the bytes it holds.
func parseDataRow(payload []byte) ([]Value, int, error) {
	r := &readBuf{b: payload}
	n := int(r.int16())
	if n < 0 {
		return nil, 0, fmt.Errorf("pgwire: DataRow with negative field count %d", n)
	}
	row := make([]Value, 0, n)
	size := 0
	for range n {
		length := int(r.int32())
		switch {
		case length == -1:
			row = append(row, Value{}) // NULL
			continue
		case length < 0:
			return nil, 0, fmt.Errorf("pgwire: DataRow field length %d", length)
		}
		row = append(row, Value{S: string(r.bytes(length)), Valid: true})
		size += length
	}
	if r.err != nil {
		return nil, 0, r.err
	}
	return row, size, nil
}

// Value is one text-format field of a query result. Valid is false for NULL.
type Value struct {
	S     string
	Valid bool
}
