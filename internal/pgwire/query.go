package pgwire

// Query runs one SQL statement over the simple-query protocol and returns
// every row as text-format values. A NULL becomes ("", false); everything
// else is (value, true).
//
// This is for pgc's own catalog lookups (pg_type, pg_attribute, pg_class) -
// small, trusted, numbers-only queries built by pgc itself. It is not meant
// for application data.
func (c *Conn) Query(sql string) ([][]Value, error) {
	c.begin()
	defer c.done()

	var q writeBuf
	q.cstring(sql)
	if err := c.sendFlush('Q', q); err != nil {
		return nil, err
	}

	var rows [][]Value
	var srvErr *ServerError
	for {
		typ, payload, err := c.recv()
		if err != nil {
			return nil, err
		}
		switch typ {
		case 'T': // RowDescription - column meta not needed here
		case 'D': // DataRow
			r := &readBuf{b: payload}
			n := int(r.int16())
			row := make([]Value, 0, n)
			for range n {
				size := int(r.int32())
				if size < 0 {
					row = append(row, Value{}) // NULL
					continue
				}
				row = append(row, Value{S: string(r.bytes(size)), Valid: true})
			}
			if r.err != nil {
				return nil, r.err
			}
			rows = append(rows, row)
		case 'C': // CommandComplete
		case 'I': // EmptyQueryResponse
		case 'E':
			srvErr = parseServerError(payload)
		case 'N': // NoticeResponse
		case 'Z': // ReadyForQuery
			if srvErr != nil {
				return nil, srvErr
			}
			return rows, nil
		}
	}
}

// Value is one text-format field of a query result. Valid is false for NULL.
type Value struct {
	S     string
	Valid bool
}
