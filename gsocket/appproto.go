package gsocket

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Application protocol message types — match the C pkt_mgr.h definitions.
const (
	// Fixed-size messages (size determined by type number).
	msgWSize  = 1  // terminal window size change
	msgIDS    = 2  // intrusion detection system event
	msgPWDReq = 3  // working directory request
	msgPing   = 16 // application-level ping
	msgPong   = 16 // application-level pong (same type, direction distinguishes)
	msgLog    = 32 // log message (max 64 bytes)
	msgStatus = 33 // status message (max 64 bytes)

	// Channel messages (variable size, type >= 128).
	chnFTData    = 131 // file transfer data
	chnFTError   = 132 // file transfer error
	chnFTSwitch  = 133 // file transfer switch (direction change)
	chnFTListRpl = 134 // file transfer list reply
	chnFTDownload = 135 // file transfer download request
	chnPWD       = 136 // working directory reply

	// Channel types (offset from message types).
	chnOffset = 128

	// Escape byte for in-band signaling.
	escapeByte = 0xfe

	// Packet sizes per message type.
	msgSizeSmall  = 4
	msgSizeTiny   = 16
	msgSizeMedium = 64
	msgSizeLarge  = 128
	msgSizeHuge   = 512
	msgSizeMax    = 4196
)

// Application-level packet structures (mirrors C pkt_mgr.h).

// AppPing is an application-level keepalive ping.
type AppPing struct {
	Flags  uint8
	_      [3]uint8
	User   [12]uint8
}

// AppPong is an application-level keepalive pong.
type AppPong struct {
	Load  uint16
	Idle  uint16
	NUsers uint8
	User  [11]uint8
}

// AppLog is a log message sent from server to client.
type AppLog struct {
	Type uint8
	Msg  [63]uint8
}

// AppStatus is a status message (e.g., no PTY available).
type AppStatus struct {
	Type uint8
	Msg  [63]uint8
}

// Log type constants.
const (
	LogTypeDefault = 0x00
	LogTypeAlert   = 0x01
	LogTypeNotice  = 0x02
	LogTypeInfo    = 0x03
)

// Status type constants.
const (
	StatusTypeNoPTY = 0x01
)

// MsgSize returns the fixed message size for a given message type.
// Returns -1 for invalid types.
func MsgSize(msgType uint8) int {
	switch {
	case msgType == 0:
		return -1
	case msgType < 16:
		return 4
	case msgType < 32:
		return 16
	case msgType < 48:
		return 64
	case msgType < 64:
		return 128
	case msgType < 80:
		return 512
	case msgType < 96:
		return 1024
	case msgType < 112:
		return 2048
	default:
		return 4196
	}
}

// AppProto handles in-band signaling over an encrypted channel.
// It encodes/decodes escape sequences that multiplex application
// messages (window size, keepalive, logs, file transfer) within
// the encrypted data stream.
type AppProto struct {
	rw           io.ReadWriter
	escRemaining int    // bytes remaining in current escape sequence
	escType      uint8  // type of current escape sequence
	inband       []byte // accumulated in-band data
	inbandLen    int
	gotChnLen    bool   // received the 2-byte channel length
	callbacks    map[uint8]MessageCallback
}

// MessageCallback receives decoded application messages.
// msgType: the message/channel type
// data: the message payload
type MessageCallback func(msgType uint8, data []byte) error

// NewAppProto creates a new application protocol handler.
func NewAppProto(rw io.ReadWriter) *AppProto {
	return &AppProto{
		rw:        rw,
		inband:    make([]byte, msgSizeMax),
		callbacks: make(map[uint8]MessageCallback),
	}
}

// OnMessage registers a callback for a specific message type.
func (ap *AppProto) OnMessage(msgType uint8, cb MessageCallback) {
	ap.callbacks[msgType] = cb
}

// OnChannel registers a callback for a channel type (message types >= 128).
func (ap *AppProto) OnChannel(chn uint8, cb MessageCallback) {
	ap.callbacks[chn+chnOffset] = cb
}

// SendMessage sends a fixed-size application message.
func (ap *AppProto) SendMessage(msgType uint8, data []byte) error {
	size := MsgSize(msgType)
	if size < 0 {
		return fmt.Errorf("invalid message type: %d", msgType)
	}
	if len(data) > size {
		return fmt.Errorf("data too large for message type %d: %d > %d", msgType, len(data), size)
	}

	pkt := make([]byte, 2+size)
	pkt[0] = escapeByte
	pkt[1] = msgType
	copy(pkt[2:], data)

	_, err := ap.rw.Write(pkt)
	return err
}

// SendChannel sends a variable-size channel message.
func (ap *AppProto) SendChannel(chn uint8, data []byte) error {
	msgType := chn + chnOffset
	pkt := make([]byte, 4+len(data))
	pkt[0] = escapeByte
	pkt[1] = msgType
	binary.BigEndian.PutUint16(pkt[2:4], uint16(len(data)))
	copy(pkt[4:], data)

	_, err := ap.rw.Write(pkt)
	return err
}

// Decode processes incoming data and extracts application messages.
// It filters out escape sequences, returning only the plaintext data
// and calling registered callbacks for any extracted messages.
//
// Usage:
//
//	plaintext, err := ap.Decode(encryptedData)
//	// plaintext is the data stream with escape sequences removed
//	// callbacks fire for any application messages found
func (ap *AppProto) Decode(src []byte) ([]byte, error) {
	dst := make([]byte, 0, len(src))
	s := src

	for len(s) > 0 {
		if ap.escRemaining > 0 {
			n, lit := ap.consumeEscape(s)
			s = s[n:]
			if lit != nil {
				dst = append(dst, *lit)
			}
			continue
		}

		if s[0] == escapeByte {
			ap.escType = 0
			ap.escRemaining = 1
			s = s[1:]
			continue
		}

		// Plain byte — copy to output.
		dst = append(dst, s[0])
		s = s[1:]
	}

	return dst, nil
}

// consumeEscape processes bytes within an escape sequence.
// Returns the number of bytes consumed from src and any literal byte to output.
func (ap *AppProto) consumeEscape(src []byte) (consumed int, literal *byte) {
	if ap.escType == 0 {
		// First byte after escape — it's the message/channel type.
		if src[0] == escapeByte {
			// Double-escape: emit a literal escape byte.
			b := byte(escapeByte)
			ap.resetEscape()
			return 1, &b
		}
		ap.escType = src[0]
		if ap.escType >= chnOffset {
			ap.escRemaining = 2 // channel length prefix
		} else {
			size := MsgSize(ap.escType)
			if size < 0 {
				return 1, nil // protocol error — reset
			}
			ap.escRemaining = size
		}
		return 1, nil
	}

	if ap.escType >= chnOffset && !ap.gotChnLen {
		// Reading 2-byte channel length.
		toRead := min(ap.escRemaining, len(src))
		copy(ap.inband[ap.inbandLen:], src[:toRead])
		ap.inbandLen += toRead
		ap.escRemaining -= toRead
		if ap.escRemaining == 0 {
			chnLen := int(binary.BigEndian.Uint16(ap.inband[:2]))
			ap.gotChnLen = true
			ap.inbandLen = 0
			if chnLen > 0 {
				ap.escRemaining = chnLen
			} else {
				ap.dispatchMessage(ap.escType, nil)
				ap.resetEscape()
			}
		}
		return toRead, nil
	}

	// Reading message/channel payload.
	toRead := min(ap.escRemaining, len(src))
	copy(ap.inband[ap.inbandLen:], src[:toRead])
	ap.inbandLen += toRead
	ap.escRemaining -= toRead
	if ap.escRemaining == 0 {
		ap.dispatchMessage(ap.escType, ap.inband[:ap.inbandLen])
		ap.resetEscape()
	}
	return toRead, nil
}

func (ap *AppProto) dispatchMessage(msgType uint8, data []byte) {
	if cb, ok := ap.callbacks[msgType]; ok {
		val := msgType
		if val >= chnOffset {
			val -= chnOffset
		}
		_ = cb(val, data)
	}
}

func (ap *AppProto) resetEscape() {
	ap.escType = 0
	ap.escRemaining = 0
	ap.inbandLen = 0
	ap.gotChnLen = false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
