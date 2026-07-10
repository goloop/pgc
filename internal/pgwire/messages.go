package pgwire

import (
	"encoding/binary"
	"fmt"
	"io"
)

// maxMessage caps a single backend message so a broken or hostile peer
// cannot make the client allocate without bound. Catalog result sets and
// row descriptions are far below this.
const maxMessage = 8 << 20 // 8 MiB

// send writes one frontend message: a type byte, the int32 length (which
// counts itself but not the type byte) and the payload. The write is
// buffered; call flush when the batch is complete.
func (c *Conn) send(typ byte, payload []byte) error {
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)+4))
	if _, err := c.w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := c.w.Write(payload)
	return err
}

// recv reads one backend message and returns its type byte and payload.
func (c *Conn) recv() (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n < 4 || n-4 > maxMessage {
		return 0, nil, fmt.Errorf("pgwire: bad message length %d", n)
	}
	payload := make([]byte, n-4)
	if _, err := io.ReadFull(c.r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

// writeBuf builds a message payload. The zero value is ready to use.
type writeBuf []byte

func (b *writeBuf) int16(v int16) {
	*b = binary.BigEndian.AppendUint16(*b, uint16(v))
}

func (b *writeBuf) int32(v int32) {
	*b = binary.BigEndian.AppendUint32(*b, uint32(v))
}

// cstring appends a NUL-terminated string.
func (b *writeBuf) cstring(s string) {
	*b = append(*b, s...)
	*b = append(*b, 0)
}

func (b *writeBuf) bytes(p []byte) {
	*b = append(*b, p...)
}

// readBuf walks a message payload. Reads past the end set err instead of
// panicking, so parsing code can check once at the end.
type readBuf struct {
	b   []byte
	err error
}

func (r *readBuf) fail() {
	if r.err == nil {
		r.err = fmt.Errorf("pgwire: message payload too short")
	}
}

func (r *readBuf) int16() int16 {
	if r.err != nil || len(r.b) < 2 {
		r.fail()
		return 0
	}
	v := int16(binary.BigEndian.Uint16(r.b))
	r.b = r.b[2:]
	return v
}

func (r *readBuf) int32() int32 {
	if r.err != nil || len(r.b) < 4 {
		r.fail()
		return 0
	}
	v := int32(binary.BigEndian.Uint32(r.b))
	r.b = r.b[4:]
	return v
}

// cstring reads a NUL-terminated string.
func (r *readBuf) cstring() string {
	if r.err != nil {
		return ""
	}
	for i, c := range r.b {
		if c == 0 {
			s := string(r.b[:i])
			r.b = r.b[i+1:]
			return s
		}
	}
	r.fail()
	return ""
}

func (r *readBuf) bytes(n int) []byte {
	if r.err != nil || n < 0 || len(r.b) < n {
		r.fail()
		return nil
	}
	p := r.b[:n]
	r.b = r.b[n:]
	return p
}
