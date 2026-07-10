package pgwire

import "fmt"

// ServerError is an ErrorResponse from the backend, decoded into its common
// fields. The five-character Code is the SQLSTATE class from the PostgreSQL
// documentation.
type ServerError struct {
	Severity string
	Code     string
	Message  string
	Detail   string
	Hint     string
	Position string // 1-based character index into the query, when given
}

// Error implements the error interface.
func (e *ServerError) Error() string {
	s := fmt.Sprintf("pgwire: %s (%s): %s", e.Severity, e.Code, e.Message)
	if e.Position != "" {
		s += " (position " + e.Position + ")"
	}
	return s
}

// parseServerError decodes an ErrorResponse payload: a sequence of
// field-type bytes each followed by a NUL-terminated value, ended by a zero
// byte. Unknown fields are skipped.
func parseServerError(payload []byte) *ServerError {
	e := &ServerError{}
	r := &readBuf{b: payload}
	for {
		if len(r.b) == 0 || r.b[0] == 0 {
			break
		}
		typ := r.b[0]
		r.b = r.b[1:]
		val := r.cstring()
		if r.err != nil {
			break
		}
		switch typ {
		case 'S':
			e.Severity = val
		case 'C':
			e.Code = val
		case 'M':
			e.Message = val
		case 'D':
			e.Detail = val
		case 'H':
			e.Hint = val
		case 'P':
			e.Position = val
		}
	}
	return e
}
