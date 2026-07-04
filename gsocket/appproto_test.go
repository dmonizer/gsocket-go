package gsocket

import (
	"bytes"
	"io"
	"testing"
)

// testConn is an io.ReadWriter backed by bytes.Buffer for testing AppProto.
type testConn struct {
	*bytes.Buffer
	readBuf *bytes.Buffer
}

func newTestConn() *testConn {
	return &testConn{
		Buffer:  new(bytes.Buffer),
		readBuf: new(bytes.Buffer),
	}
}

func (c *testConn) Read(p []byte) (int, error) {
	return c.readBuf.Read(p)
}

// injectData simulates data arriving from the network.
func (c *testConn) injectData(data []byte) {
	c.readBuf.Write(data)
}

func TestAppProtoPlainData(t *testing.T) {
	conn := newTestConn()
	ap := NewAppProto(conn)

	// Plain data without escape sequences should pass through.
	plaintext := []byte("Hello, World! This is plain data.")
	conn.injectData(plaintext)

	result, err := ap.Decode(plaintext)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if !bytes.Equal(result, plaintext) {
		t.Errorf("plain data passthrough failed: got %q, want %q", result, plaintext)
	}
}

func TestAppProtoEscapeLiteral(t *testing.T) {
	conn := newTestConn()
	ap := NewAppProto(conn)

	// Double escape should decode to a single escape byte.
	input := []byte{escapeByte, escapeByte}
	conn.injectData(input)

	result, err := ap.Decode(input)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	expected := []byte{escapeByte}
	if !bytes.Equal(result, expected) {
		t.Errorf("escape literal: got %x, want %x", result, expected)
	}
}

func TestAppProtoFixedMessage(t *testing.T) {
	conn := newTestConn()
	ap := NewAppProto(conn)

	received := make(chan []byte, 1)
	ap.OnMessage(msgPing, func(msgType uint8, data []byte) error {
		received <- data
		return nil
	})

	// Send a ping message (type=16, size=16 bytes).
	input := make([]byte, 18) // 1 escape + 1 type + 16 data
	input[0] = escapeByte
	input[1] = msgPing
	for i := 2; i < 18; i++ {
		input[i] = byte(i)
	}
	conn.injectData(input)

	result, err := ap.Decode(input)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	// The escape sequence should be consumed, leaving no plaintext.
	if len(result) != 0 {
		t.Errorf("expected empty result, got %x", result)
	}

	// Callback should have been called with the message data.
	select {
	case data := <-received:
		if len(data) != 16 {
			t.Errorf("received message size = %d, want 16", len(data))
		}
	default:
		t.Error("message callback was not called")
	}
}

func TestAppProtoChannelMessage(t *testing.T) {
	conn := newTestConn()
	ap := NewAppProto(conn)

	received := make(chan []byte, 1)
	ap.OnChannel(chnFTData-chnOffset, func(msgType uint8, data []byte) error {
		received <- data
		return nil
	})

	// Send a channel message: [ESC][CHN][2-byte len][data].
	payload := []byte("file transfer data here")
	input := make([]byte, 4+len(payload))
	input[0] = escapeByte
	input[1] = chnFTData          // channel type (>= 128)
	input[2] = byte(len(payload) >> 8) // length high byte
	input[3] = byte(len(payload))      // length low byte
	copy(input[4:], payload)

	conn.injectData(input)

	result, err := ap.Decode(input)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if len(result) != 0 {
		t.Errorf("expected empty result, got %x", result)
	}

	select {
	case data := <-received:
		if !bytes.Equal(data, payload) {
			t.Errorf("received %q, want %q", data, payload)
		}
	default:
		t.Error("channel callback was not called")
	}
}

func TestAppProtoMixedData(t *testing.T) {
	conn := newTestConn()
	ap := NewAppProto(conn)

	messages := make(chan []byte, 10)
	ap.OnMessage(msgPing, func(msgType uint8, data []byte) error {
		messages <- data
		return nil
	})

	// Mix plain data with a message in between.
	input := []byte{
		'H', 'e', 'l', 'l', 'o', ' ', // plain text
		escapeByte, msgPing, // start of ping message
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, // 16 bytes ping data
		'W', 'o', 'r', 'l', 'd', // more plain text
	}
	conn.injectData(input)

	result, err := ap.Decode(input)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	expected := []byte("Hello World")
	if !bytes.Equal(result, expected) {
		t.Errorf("got %q, want %q", result, expected)
	}

	// Should have received one message.
	select {
	case <-messages:
		// OK
	default:
		t.Error("message callback was not called")
	}
}

func TestAppProtoMsgSize(t *testing.T) {
	tests := []struct {
		msgType uint8
		wantSize int
	}{
		{0, -1},
		{1, 4},
		{15, 4},
		{16, 16},
		{31, 16},
		{32, 64},
		{47, 64},
		{48, 128},
		{63, 128},
		{64, 512},
		{79, 512},
		{80, 1024},
		{95, 1024},
		{96, 2048},
		{111, 2048},
		{112, 4196},
		{200, 4196},
	}

	for _, tt := range tests {
		t.Run(string(rune(tt.msgType)), func(t *testing.T) {
			size := MsgSize(tt.msgType)
			if size != tt.wantSize {
				t.Errorf("MsgSize(%d) = %d, want %d", tt.msgType, size, tt.wantSize)
			}
		})
	}
}

func TestAppProtoMessageCallbackMatching(t *testing.T) {
	conn := newTestConn()
	ap := NewAppProto(conn)

	// Register callbacks for two different message types.
	// Note: ping and pong share the same type (16) — they're distinguished
	// by direction. Use different message types for this test.
	wsizeCalled := false
	logCalled := false

	ap.OnMessage(msgWSize, func(msgType uint8, data []byte) error {
		wsizeCalled = true
		return nil
	})
	ap.OnMessage(msgLog, func(msgType uint8, data []byte) error {
		logCalled = true
		return nil
	})

	// Send a WSize message (type=1, size=4).
	input := make([]byte, 6)
	input[0] = escapeByte
	input[1] = msgWSize
	conn.injectData(input)

	_, err := ap.Decode(input)
	if err != nil {
		t.Fatalf("Decode failed: %v", err)
	}

	if !wsizeCalled {
		t.Error("wsize callback was not called")
	}
	if logCalled {
		t.Error("log callback was called but shouldn't have been")
	}
}

func TestAppProtoWriteMessage(t *testing.T) {
	var buf bytes.Buffer
	ap := NewAppProto(&rwOnly{w: &buf})

	// Send a ping message.
	data := make([]byte, 16)
	data[0] = 0x42
	if err := ap.SendMessage(msgPing, data); err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}

	result := buf.Bytes()
	if len(result) != 18 {
		t.Errorf("encoded size = %d, want 18", len(result))
	}
	if result[0] != escapeByte {
		t.Errorf("first byte = 0x%02x, want 0xfe", result[0])
	}
	if result[1] != msgPing {
		t.Errorf("type byte = %d, want %d", result[1], msgPing)
	}
}

func TestAppProtoWriteChannelMessage(t *testing.T) {
	var buf bytes.Buffer
	ap := NewAppProto(&rwOnly{w: &buf})

	payload := []byte("test data")
	if err := ap.SendChannel(chnFTData-chnOffset, payload); err != nil {
		t.Fatalf("SendChannel failed: %v", err)
	}

	result := buf.Bytes()
	if len(result) != 4+len(payload) {
		t.Errorf("encoded size = %d, want %d", len(result), 4+len(payload))
	}
	if result[0] != escapeByte {
		t.Errorf("first byte = 0x%02x, want 0xfe", result[0])
	}
	if result[1] != chnFTData {
		t.Errorf("type byte = %d, want %d", result[1], chnFTData)
	}
}

// rwOnly is a minimal io.ReadWriter for testing.
type rwOnly struct {
	w io.Writer
	r io.Reader
}

func (rw *rwOnly) Read(p []byte) (int, error)  { return rw.r.Read(p) }
func (rw *rwOnly) Write(p []byte) (int, error) { return rw.w.Write(p) }
