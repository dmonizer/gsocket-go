package gsocket

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestGSRNPacketStructSizes(t *testing.T) {
	// Verify packet struct sizes match the C definitions.
	tests := []struct {
		name     string
		expected int
		actual   int
	}{
		{"gsListenPacket", gsListenSize, binary.Size(gsListenPacket{})},
		{"gsConnectPacket", gsConnectSize, binary.Size(gsConnectPacket{})},
		{"gsPingPacket", gsPingSize, binary.Size(gsPingPacket{})},
		{"gsPongPacket", gsPongSize, binary.Size(gsPongPacket{})},
		{"gsStartPacket", gsStartSize, binary.Size(gsStartPacket{})},
		{"gsAcceptPacket", gsAcceptSize, binary.Size(gsAcceptPacket{})},
		{"gsStatusPacket", gsStatusSize, binary.Size(gsStatusPacket{})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.actual != tt.expected {
				t.Errorf("%s size = %d, want %d", tt.name, tt.actual, tt.expected)
			}
		})
	}
}

func TestGSRNListenPacketMarshaling(t *testing.T) {
	var addr Addr
	copy(addr[:], []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
		0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f})

	var token [TokenSize]byte
	copy(token[:], []byte{0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
		0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f})

	pkt := gsListenPacket{
		Type:         pktTypeListen,
		VersionMajor: protoVersionMajor,
		VersionMinor: protoVersionMinor,
	}
	copy(pkt.Token[:], token[:])
	copy(pkt.Addr[:], addr[:])

	var buf bytes.Buffer
	if err := sendPacket(&buf, &pkt); err != nil {
		t.Fatalf("sendPacket failed: %v", err)
	}

	if buf.Len() != gsListenSize {
		t.Errorf("marshaled size = %d, want %d", buf.Len(), gsListenSize)
	}

	// First byte should be LISTEN type.
	data := buf.Bytes()
	if data[0] != pktTypeListen {
		t.Errorf("first byte = 0x%02x, want 0x%02x", data[0], pktTypeListen)
	}
}

func TestGSRNConnectPacketMarshaling(t *testing.T) {
	var addr Addr
	copy(addr[:], bytes.Repeat([]byte{0xaa}, AddrSize))

	pkt := gsConnectPacket{
		Type:         pktTypeConnect,
		VersionMajor: protoVersionMajor,
		VersionMinor: protoVersionMinor,
		Flags:        flagProtoLowLatency,
	}
	copy(pkt.Addr[:], addr[:])

	var buf bytes.Buffer
	if err := sendPacket(&buf, &pkt); err != nil {
		t.Fatalf("sendPacket failed: %v", err)
	}

	if buf.Len() != gsConnectSize {
		t.Errorf("marshaled size = %d, want %d", buf.Len(), gsConnectSize)
	}

	data := buf.Bytes()
	if data[0] != pktTypeConnect {
		t.Errorf("first byte = 0x%02x, want 0x%02x", data[0], pktTypeConnect)
	}
}

func TestParseStart(t *testing.T) {
	tests := []struct {
		name        string
		flags       uint8
		wantServer  bool
		wantErr     bool
	}{
		{"server", flagStartServer, true, false},
		{"client", flagStartClient, false, false},
		{"neither", 0, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := []byte{pktTypeStart, tt.flags}
			actAsServer, err := ParseStart(payload)

			if tt.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if actAsServer != tt.wantServer {
				t.Errorf("actAsServer = %v, want %v", actAsServer, tt.wantServer)
			}
		})
	}
}

func TestParseStatus(t *testing.T) {
	tests := []struct {
		name    string
		errType uint8
		code    uint8
		wantErr bool
	}{
		{"bad auth fatal", statusTypeFatal, statusCodeBadAuth, true},
		{"conn refused fatal", statusTypeFatal, statusCodeConnRefused, true},
		{"idle timeout fatal", statusTypeFatal, statusCodeIdleTimeout, true},
		{"server ok warn", statusTypeWarn, statusCodeServerOK, false},
		{"unknown code", statusTypeFatal, 0xff, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := []byte{pktTypeStatus, tt.errType, tt.code, 0}
			// Pad to 32 bytes with zeros.
			for len(payload) < gsStatusSize {
				payload = append(payload, 0)
			}
			err := ParseStatus(payload)

			if tt.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestStatusErrorMapping(t *testing.T) {
	// Verify that each status code maps to the correct error.
	tests := []struct {
		code    uint8
		wantErr error
	}{
		{statusCodeBadAuth, ErrGSRNAuthFailed},
		{statusCodeConnRefused, ErrGSRNConnRefused},
		{statusCodeIdleTimeout, ErrGSRNIdleTimeout},
		{statusCodeConnDenied, ErrGSRNConnDenied},
		{statusCodeProtoError, ErrGSRNProtoError},
		{statusCodeNetError, ErrGSRNNetError},
		{statusCodeNeedUpdate, ErrGSRNNeedUpdate},
	}

	for _, tt := range tests {
		t.Run(tt.wantErr.Error(), func(t *testing.T) {
			err := statusCodeToError(tt.code, "")
			if err != tt.wantErr {
				t.Errorf("statusCodeToError(%d) = %v, want %v", tt.code, err, tt.wantErr)
			}
		})
	}
}

func TestGenerateRandomToken(t *testing.T) {
	token1, err := generateRandomToken()
	if err != nil {
		t.Fatalf("generateRandomToken failed: %v", err)
	}

	token2, err := generateRandomToken()
	if err != nil {
		t.Fatalf("generateRandomToken failed: %v", err)
	}

	// Tokens should be different (extremely unlikely to collide).
	if token1 == token2 {
		t.Error("two random tokens are identical")
	}
}
