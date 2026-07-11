package pgwire

import "fmt"

// Column describes one result column of a statement, as reported by the
// backend's RowDescription. TableOID and Attnum are non-zero only when the
// column comes directly from a table; expressions have no origin and get
// zeros.
type Column struct {
	Name     string
	TypeOID  uint32
	TableOID uint32
	Attnum   int16
	TypeMod  int32
}

// Statement is what the server knows about a parsed (never executed) query:
// the type of every $N parameter and the shape of the result set. Columns is
// nil for statements that return no rows.
type Statement struct {
	ParamOIDs []uint32
	Columns   []Column
}

// Describe asks the backend to parse and describe query without executing
// it: Parse/Describe/Sync on the unnamed prepared statement. The server does
// all type inference; a query that would not run comes back as *ServerError
// with the exact position of the problem.
func (c *Conn) Describe(query string) (*Statement, error) {
	c.begin()
	defer c.done()

	var parse writeBuf
	parse.cstring("") // unnamed statement
	parse.cstring(query)
	parse.int16(0) // no pre-specified parameter types
	if err := c.send('P', parse); err != nil {
		return nil, err
	}

	var describe writeBuf
	describe = append(describe, 'S') // describe a statement
	describe.cstring("")
	if err := c.send('D', describe); err != nil {
		return nil, err
	}

	if err := c.send('S', nil); err != nil { // Sync
		return nil, err
	}
	if err := c.w.Flush(); err != nil {
		return nil, err
	}

	st := &Statement{}
	var srvErr *ServerError
	for {
		typ, payload, err := c.recv()
		if err != nil {
			return nil, err
		}
		switch typ {
		case '1': // ParseComplete
		case 't': // ParameterDescription
			r := &readBuf{b: payload}
			n := int(r.int16())
			if n < 0 {
				return nil, fmt.Errorf(
					"pgwire: ParameterDescription with negative count %d", n)
			}
			for range n {
				st.ParamOIDs = append(st.ParamOIDs, uint32(r.int32()))
			}
			if r.err != nil {
				return nil, r.err
			}
		case 'T': // RowDescription
			r := &readBuf{b: payload}
			n := int(r.int16())
			if n < 0 {
				return nil, fmt.Errorf(
					"pgwire: RowDescription with negative count %d", n)
			}
			for range n {
				col := Column{Name: r.cstring()}
				col.TableOID = uint32(r.int32())
				col.Attnum = r.int16()
				col.TypeOID = uint32(r.int32())
				_ = r.int16() // type length
				col.TypeMod = r.int32()
				_ = r.int16() // format code
				st.Columns = append(st.Columns, col)
			}
			if r.err != nil {
				return nil, r.err
			}
		case 'n': // NoData - statement returns no rows
		case 'E':
			srvErr = parseServerError(payload)
		case 'N': // NoticeResponse
		case 'Z': // ReadyForQuery - conversation over
			if srvErr != nil {
				return nil, srvErr
			}
			return st, nil
		}
	}
}
