package gsocket

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

func (ft *FileTransfer) receive(channel uint8, data []byte) error {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	switch channel {
	case chnFTPut:
		if !ft.server {
			return errFTPacket
		}
		return ft.receivePut(data)
	case chnFTAccept:
		if ft.server {
			return errFTPacket
		}
		return ft.receiveAccept(data)
	case chnFTListReq:
		if !ft.server {
			return errFTPacket
		}
		return ft.receiveListRequest(data)
	case chnFTListRpl:
		if ft.server {
			return errFTPacket
		}
		return ft.receiveListReply(data)
	case chnFTDownload:
		if !ft.server {
			return errFTPacket
		}
		return ft.receiveDownload(data)
	case chnFTSwitch:
		return ft.receiveSwitch(data)
	case chnFTData:
		return ft.receiveData(data)
	case chnFTError:
		return ft.receiveStatus(data)
	}
	return errFTPacket
}

func (ft *FileTransfer) receivePut(data []byte) error {
	name, err := ftName(data, 32)
	if err != nil {
		return err
	}
	id := binary.BigEndian.Uint32(data[0:4])
	size := ftUint64(data[8:16])
	if size < 0 || ft.incoming[id] != nil {
		return errFTPacket
	}
	path, err := ftDestination(ft.cwd(), name)
	if err != nil {
		ft.enqueue(ftErrorPacket(id, ftErrInvalid, ""))
		return nil
	}
	meta := ftMeta{ftFileMode(binary.BigEndian.Uint32(data[4:8])), time.Unix(int64(binary.BigEndian.Uint32(data[16:20])), 0)}
	if data[20]&ftFlagDir != 0 {
		if err := os.MkdirAll(path, 0755); err != nil {
			ft.enqueue(ftErrorPacket(id, ftErrorCode(err), ""))
			return nil
		}
		ftApplyMeta(path, meta)
		ft.dirs[path] = meta
		return nil // C sends no ACCEPT for directory offers.
	}
	var offset int64
	if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() {
			ft.enqueue(ftErrorPacket(id, ftErrBadFile, ""))
			return nil
		}
		offset = info.Size()
	} else if !os.IsNotExist(err) {
		ft.enqueue(ftErrorPacket(id, ftErrorCode(err), ""))
		return nil
	}
	f := &ftFile{id: id, name: name, path: path, size: size, offset: offset, meta: meta}
	ft.incoming[id] = f
	ft.enqueue(ftOffsetPacket(chnFTAccept, id, offset))
	return nil
}

func (ft *FileTransfer) receiveAccept(data []byte) error {
	if len(data) < 24 {
		return errFTPacket
	}
	id := binary.BigEndian.Uint32(data[0:4])
	f := ft.pending[id]
	if f == nil {
		return nil
	}
	f.offset = ftUint64(data[8:16])
	if f.offset < 0 {
		return errFTPacket
	}
	ft.ready = append(ft.ready, f)
	ft.signal()
	return nil
}

func (ft *FileTransfer) receiveListRequest(data []byte) error {
	pattern, err := ftName(data, 4)
	if err != nil {
		return err
	}
	id := binary.BigEndian.Uint32(data[0:4])
	sources, err := ftSources(pattern, ft.cwd())
	if len(sources) == 0 {
		ft.enqueue(ftErrorPacket(id, ftErrNoEnt, ""))
		return nil
	}
	if err != nil {
		ft.note(fmt.Sprintf("list: %v", err))
	}
	for i, src := range sources {
		flags := uint8(0)
		size := src.info.Size()
		if src.info.IsDir() {
			flags |= ftFlagDir
			size = 0
		}
		if i == len(sources)-1 {
			flags |= ftFlagLast
		}
		pkt, err := ftListReplyPacket(id, ftModeBits(src.info.Mode()), size, uint32(src.info.ModTime().Unix()), flags, src.wire)
		if err != nil {
			ft.enqueue(ftErrorPacket(id, ftErrInvalid, ""))
			return nil
		}
		ft.enqueue(pkt)
	}
	return nil
}

func (ft *FileTransfer) receiveListReply(data []byte) error {
	name, err := ftName(data, 40)
	if err != nil {
		return err
	}
	id := binary.BigEndian.Uint32(data[0:4])
	root, ok := ft.requests[id]
	if !ok {
		return nil
	}
	if data[28]&ftFlagLast != 0 {
		delete(ft.requests, id)
	}
	path, err := ftDestination(root, name)
	if err != nil {
		ft.stats.Errors++
		ft.note(fmt.Sprintf("get %s: unsafe file name", name))
		return nil
	}
	size := ftUint64(data[16:24])
	if size < 0 {
		return errFTPacket
	}
	meta := ftMeta{ftFileMode(binary.BigEndian.Uint32(data[12:16])), time.Unix(int64(binary.BigEndian.Uint32(data[24:28])), 0)}
	if data[28]&ftFlagDir != 0 {
		if err := os.MkdirAll(path, 0755); err != nil {
			ft.stats.Errors++
			ft.note(fmt.Sprintf("mkdir %s: %v", path, err))
		} else {
			ftApplyMeta(path, meta)
			ft.dirs[path] = meta
		}
		return nil
	}
	var offset int64
	if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() {
			ft.stats.Errors++
			ft.note(fmt.Sprintf("get %s: destination is not a regular file", path))
			return nil
		}
		if info.Size() == size {
			ft.stats.Success++
			ft.note(fmt.Sprintf("%s already complete", path))
			return nil
		}
		if info.Size() < size {
			offset = info.Size()
		}
	} else if !os.IsNotExist(err) {
		ft.stats.Errors++
		ft.note(fmt.Sprintf("get %s: %v", path, err))
		return nil
	}
	fileID := ft.newID()
	f := &ftFile{id: fileID, name: ftRelativeName(name), path: path, size: size, offset: offset, meta: meta}
	ft.incoming[fileID] = f
	ft.pending[fileID] = f
	pkt, err := ftDownloadPacket(fileID, offset, name)
	if err != nil {
		delete(ft.incoming, fileID)
		delete(ft.pending, fileID)
		ft.stats.Errors++
		return nil
	}
	ft.enqueue(pkt)
	return nil
}

func (ft *FileTransfer) receiveDownload(data []byte) error {
	name, err := ftName(data, 24)
	if err != nil {
		return err
	}
	id := binary.BigEndian.Uint32(data[0:4])
	offset := ftUint64(data[8:16])
	if offset < 0 || ft.pending[id] != nil {
		return errFTPacket
	}
	path := name
	if !filepath.IsAbs(path) {
		path = filepath.Join(ft.cwd(), path)
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		ft.enqueue(ftErrorPacket(id, ftErrorCode(err), ""))
		return nil
	}
	f := &ftFile{id: id, name: ftRelativeName(name), path: path, size: info.Size(), offset: offset,
		meta: ftMeta{info.Mode(), info.ModTime()}}
	ft.pending[id] = f
	ft.ready = append(ft.ready, f)
	ft.signal()
	return nil
}

func (ft *FileTransfer) receiveSwitch(data []byte) error {
	if len(data) < 16 {
		return errFTPacket
	}
	id := binary.BigEndian.Uint32(data[0:4])
	offset := ftUint64(data[8:16])
	f := ft.incoming[id]
	if f == nil {
		return nil
	}
	if offset < 0 || offset > f.size {
		ft.receiveError(f, ftErrBadSize, fmt.Errorf("invalid resume offset"))
		return nil
	}
	if ft.activeIn != nil && ft.activeIn != f {
		ft.activeIn.close()
		ft.activeIn = nil
	}
	if offset > 0 {
		info, err := os.Stat(f.path)
		if err != nil || info.Size() != offset {
			ft.receiveError(f, ftErrBadSize, fmt.Errorf("destination size changed"))
			return nil
		}
	} else if err := os.MkdirAll(filepath.Dir(f.path), 0755); err != nil {
		ft.receiveError(f, ftErrorCode(err), err)
		return nil
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if offset > 0 {
		flags = os.O_WRONLY | os.O_APPEND
	}
	file, err := os.OpenFile(f.path, flags, 0600)
	if err != nil {
		ft.receiveError(f, ftErrorCode(err), err)
		return nil
	}
	f.file, f.position, f.started = file, offset, time.Now()
	if offset == f.size {
		ft.completeIncoming(f)
	} else {
		ft.activeIn = f
	}
	return nil
}

func (ft *FileTransfer) receiveData(data []byte) error {
	f := ft.activeIn
	if f == nil {
		return nil
	} // C ignores data after an error or completion.
	if int64(len(data)) > f.size-f.position {
		ft.receiveError(f, ftErrBadSize, fmt.Errorf("too much file data"))
		return nil
	}
	n, err := f.file.Write(data)
	f.position += int64(n)
	f.transferred += int64(n)
	if err != nil || n != len(data) {
		if err == nil {
			err = io.ErrShortWrite
		}
		ft.receiveError(f, ftErrBadFile, fmt.Errorf("write %s: %w", f.path, err))
		return nil
	}
	ft.progress(f)
	if f.position == f.size {
		ft.completeIncoming(f)
	}
	return nil
}

func (ft *FileTransfer) completeIncoming(f *ftFile) {
	f.close()
	ftApplyMeta(f.path, f.meta)
	ft.restoreDirs(f.path)
	delete(ft.incoming, f.id)
	if ft.activeIn == f {
		ft.activeIn = nil
	}
	if ft.server {
		ft.enqueue(ftErrorPacket(f.id, ftErrCompleted, ""))
	} else {
		delete(ft.pending, f.id)
		ft.finish(f, nil)
	}
}

func (ft *FileTransfer) receiveError(f *ftFile, code uint8, err error) {
	f.close()
	delete(ft.incoming, f.id)
	delete(ft.pending, f.id)
	if ft.activeIn == f {
		ft.activeIn = nil
	}
	ft.enqueue(ftErrorPacket(f.id, code, ""))
	if !ft.server {
		ft.finish(f, err)
	}
}

func (ft *FileTransfer) receiveStatus(data []byte) error {
	if len(data) < 9 || data[len(data)-1] != 0 {
		return errFTPacket
	}
	id := binary.BigEndian.Uint32(data[0:4])
	code := data[4]
	if root, ok := ft.requests[id]; ok {
		delete(ft.requests, id)
		ft.stats.Errors++
		ft.note(fmt.Sprintf("get in %s: remote error %d", root, code))
		return nil
	}
	f := ft.pending[id]
	if f == nil {
		f = ft.incoming[id]
	}
	if f == nil {
		return nil
	}
	f.close()
	delete(ft.pending, id)
	delete(ft.incoming, id)
	if ft.activeOut == f {
		ft.activeOut = nil
	}
	if ft.activeIn == f {
		ft.activeIn = nil
	}
	if !ft.server {
		if code == ftErrCompleted || code == ftErrNoData {
			ft.finish(f, nil)
		} else {
			ft.finish(f, fmt.Errorf("remote transfer error %d", code))
		}
	}
	return nil
}
