package substrate

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// A minimal RFC 6455 websocket: enough for JSON-RPC text messages, without
// extensions (none are offered, so none are negotiated).

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// WSAccept is the Sec-WebSocket-Accept value for a Sec-WebSocket-Key.
func WSAccept(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// NewWSKey returns a random Sec-WebSocket-Key.
func NewWSKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return base64.StdEncoding.EncodeToString(b[:])
}

const (
	opCont   = 0x0
	opText   = 0x1
	opBinary = 0x2
	opClose  = 0x8
	opPing   = 0x9
	opPong   = 0xA
)

// MaxMessage bounds one reassembled message (runtime metadata is a few MiB).
const MaxMessage = 64 << 20

// Frame is one websocket frame header.
type Frame struct {
	Fin    bool
	Op     byte
	Masked bool
	Mask   [4]byte
	Len    uint64
	Raw    []byte // the header bytes exactly as read (incl. mask key)
}

// ReadFrameHeader reads a frame header.
func ReadFrameHeader(r io.Reader) (Frame, error) {
	var f Frame
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return f, err
	}
	f.Raw = append(f.Raw, h[:]...)
	f.Fin = h[0]&0x80 != 0
	f.Op = h[0] & 0x0f
	f.Masked = h[1]&0x80 != 0
	l := uint64(h[1] & 0x7f)
	switch l {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return f, err
		}
		f.Raw = append(f.Raw, b[:]...)
		l = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return f, err
		}
		f.Raw = append(f.Raw, b[:]...)
		l = binary.BigEndian.Uint64(b[:])
	}
	f.Len = l
	if f.Masked {
		if _, err := io.ReadFull(r, f.Mask[:]); err != nil {
			return f, err
		}
		f.Raw = append(f.Raw, f.Mask[:]...)
	}
	return f, nil
}

// Unmask XORs b with the frame's mask key (in place).
func (f Frame) Unmask(b []byte) {
	if !f.Masked {
		return
	}
	for i := range b {
		b[i] ^= f.Mask[i%4]
	}
}

// WriteFrame writes one frame; client frames (mask=true) get a random key.
func WriteFrame(w io.Writer, op byte, payload []byte, mask bool) error {
	hdr := []byte{0x80 | op}
	mb := byte(0)
	if mask {
		mb = 0x80
	}
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, mb|byte(n))
	case n <= 0xffff:
		hdr = append(hdr, mb|126)
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(n))
	default:
		hdr = append(hdr, mb|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	body := payload
	if mask {
		var key [4]byte
		_, _ = rand.Read(key[:])
		hdr = append(hdr, key[:]...)
		body = make([]byte, n)
		for i := range payload {
			body[i] = payload[i] ^ key[i%4]
		}
	}
	_, err := w.Write(append(hdr, body...))
	return err
}

// WSConn is a client websocket to an upstream node, opened over the mixnet.
type WSConn struct {
	net.Conn
	R        *bufio.Reader // may hold bytes already read past the handshake
	Upstream string        // wss:// URL
	wmu      sync.Mutex
}

// Send writes a text message (masked, as a client must).
func (c *WSConn) Send(msg []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return WriteFrame(c.Conn, opText, msg, true)
}

// ErrClosed is returned when the upstream closes the websocket.
var ErrClosed = errors.New("upstream closed the websocket")

// Recv returns the next data message, answering pings on the way.
func (c *WSConn) Recv() ([]byte, error) {
	var msg []byte
	for {
		f, err := ReadFrameHeader(c.R)
		if err != nil {
			return nil, err
		}
		if f.Len > MaxMessage || uint64(len(msg))+f.Len > MaxMessage {
			return nil, fmt.Errorf("websocket message over %d bytes", MaxMessage)
		}
		p := make([]byte, f.Len)
		if _, err := io.ReadFull(c.R, p); err != nil {
			return nil, err
		}
		f.Unmask(p)
		switch f.Op {
		case opPing:
			c.wmu.Lock()
			err := WriteFrame(c.Conn, opPong, p, true)
			c.wmu.Unlock()
			if err != nil {
				return nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			return nil, ErrClosed
		case opText, opBinary, opCont:
			msg = append(msg, p...)
			if f.Fin {
				return msg, nil
			}
		default:
			return nil, fmt.Errorf("unknown websocket opcode %d", f.Op)
		}
	}
}

// CloseWS sends a close frame and closes the stream.
func (c *WSConn) CloseWS() {
	c.wmu.Lock()
	_ = WriteFrame(c.Conn, opClose, []byte{0x03, 0xe8}, true)
	c.wmu.Unlock()
	c.Conn.Close()
}
