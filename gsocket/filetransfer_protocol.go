package gsocket

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
)

// These layouts match tools/filetransfer.h and tools/pkt_mgr.h in C beta.
const (
	ftMaxPayload = 2048
	ftFlagLast   = 1
	ftFlagDir    = 2

	ftErrUnknown   = 0
	ftErrPerm      = 1
	ftErrNoEnt     = 2
	ftErrBadSize   = 3
	ftErrBadFile   = 9
	ftErrNoData    = 10
	ftErrInvalid   = 11
	ftErrCompleted = 128
)

var errFTPacket = errors.New("invalid file transfer packet")

type ftPacket struct {
	channel uint8
	payload []byte
}

func ftNamedPayload(header []byte, name string) ([]byte, error) {
	if name == "" || strings.IndexByte(name, 0) >= 0 || len(header)+len(name)+1 > ftMaxPayload {
		return nil, fmt.Errorf("%w: invalid or overlong name", errFTPacket)
	}
	return append(append(header, name...), 0), nil
}

func ftName(data []byte, headerLen int) (string, error) {
	if len(data) <= headerLen || data[len(data)-1] != 0 || strings.IndexByte(string(data[headerLen:len(data)-1]), 0) >= 0 {
		return "", errFTPacket
	}
	name := string(data[headerLen : len(data)-1])
	if name == "" {
		return "", errFTPacket
	}
	return name, nil
}

func ftPutPacket(id uint32, mode uint32, size int64, mtime uint32, flags uint8, name string) (ftPacket, error) {
	h := make([]byte, 32)
	binary.BigEndian.PutUint32(h[0:4], id)
	binary.BigEndian.PutUint32(h[4:8], mode&07777)
	binary.BigEndian.PutUint64(h[8:16], uint64(size))
	binary.BigEndian.PutUint32(h[16:20], mtime)
	h[20] = flags
	p, err := ftNamedPayload(h, name)
	return ftPacket{chnFTPut, p}, err
}

func ftModeBits(mode os.FileMode) uint32 {
	bits := uint32(mode.Perm())
	if mode&os.ModeSetuid != 0 {
		bits |= 04000
	}
	if mode&os.ModeSetgid != 0 {
		bits |= 02000
	}
	if mode&os.ModeSticky != 0 {
		bits |= 01000
	}
	return bits
}

func ftFileMode(bits uint32) os.FileMode {
	mode := os.FileMode(bits & 0777)
	if bits&04000 != 0 {
		mode |= os.ModeSetuid
	}
	if bits&02000 != 0 {
		mode |= os.ModeSetgid
	}
	if bits&01000 != 0 {
		mode |= os.ModeSticky
	}
	return mode
}

func ftListRequestPacket(id uint32, pattern string) (ftPacket, error) {
	h := make([]byte, 4)
	binary.BigEndian.PutUint32(h, id)
	p, err := ftNamedPayload(h, pattern)
	return ftPacket{chnFTListReq, p}, err
}

func ftListReplyPacket(id uint32, mode uint32, size int64, mtime uint32, flags uint8, name string) (ftPacket, error) {
	h := make([]byte, 40)
	binary.BigEndian.PutUint32(h[0:4], id)
	binary.BigEndian.PutUint32(h[12:16], mode&07777)
	binary.BigEndian.PutUint64(h[16:24], uint64(size))
	binary.BigEndian.PutUint32(h[24:28], mtime)
	h[28] = flags
	p, err := ftNamedPayload(h, name)
	return ftPacket{chnFTListRpl, p}, err
}

func ftDownloadPacket(id uint32, offset int64, name string) (ftPacket, error) {
	h := make([]byte, 24)
	binary.BigEndian.PutUint32(h[0:4], id)
	binary.BigEndian.PutUint64(h[8:16], uint64(offset))
	p, err := ftNamedPayload(h, name)
	return ftPacket{chnFTDownload, p}, err
}

func ftOffsetPacket(channel uint8, id uint32, offset int64) ftPacket {
	h := make([]byte, 16)
	if channel == chnFTAccept {
		h = make([]byte, 24)
	}
	binary.BigEndian.PutUint32(h[0:4], id)
	binary.BigEndian.PutUint64(h[8:16], uint64(offset))
	return ftPacket{channel, h}
}

func ftErrorPacket(id uint32, code uint8, detail string) ftPacket {
	if len(detail) > ftMaxPayload-9 {
		detail = detail[:ftMaxPayload-9]
	}
	h := make([]byte, 8)
	binary.BigEndian.PutUint32(h[0:4], id)
	h[4] = code
	p, _ := ftNamedPayload(h, detail)
	if p == nil { // An empty error string is valid in the C protocol.
		p = append(h, 0)
	}
	return ftPacket{chnFTError, p}
}
