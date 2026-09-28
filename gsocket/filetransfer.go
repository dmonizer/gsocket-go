package gsocket

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileTransfer runs the C beta file-transfer protocol inside one interactive
// connection. A single sender goroutine serializes queued control packets and
// file data; incoming packets are handled by AppProto.Decode's reader.
type FileTransfer struct {
	app    *AppProto
	server bool
	cwd    func() string
	report func(string)

	mu        sync.Mutex
	wake      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	nextID    uint32
	queue     []ftPacket
	requests  map[uint32]string  // LIST id -> local destination directory
	pending   map[uint32]*ftFile // offers and downloads awaiting completion
	incoming  map[uint32]*ftFile // files accepted for incoming DATA
	ready     []*ftFile          // accepted sources awaiting SWITCH
	activeIn  *ftFile
	activeOut *ftFile
	dirs      map[string]ftMeta
	stats     TransferStats
}

type ftMeta struct {
	mode  os.FileMode
	mtime time.Time
}

type ftFile struct {
	id          uint32
	name        string
	path        string
	size        int64
	offset      int64 // existing size on destination
	position    int64 // bytes read or written at this end
	meta        ftMeta
	file        *os.File
	started     time.Time
	transferred int64
	lastReport  time.Time
}

// TransferStats aggregates completed transfers in one connection.
type TransferStats struct {
	Success int
	Errors  int
	Bytes   int64
	Elapsed time.Duration
}

func NewFileTransfer(app *AppProto, server bool, cwd func() string, report func(string)) *FileTransfer {
	ft := &FileTransfer{
		app: app, server: server, cwd: cwd, report: report,
		wake: make(chan struct{}, 1), done: make(chan struct{}),
		requests: make(map[uint32]string), pending: make(map[uint32]*ftFile),
		incoming: make(map[uint32]*ftFile), dirs: make(map[string]ftMeta),
	}
	for _, ch := range []uint8{chnFTPut, chnFTAccept, chnFTListReq, chnFTData, chnFTError, chnFTSwitch, chnFTListRpl, chnFTDownload} {
		channel := ch
		app.OnChannel(channel-chnOffset, func(_ uint8, data []byte) error {
			return ft.receive(channel, data)
		})
	}
	go ft.run()
	return ft
}

func (ft *FileTransfer) Close() {
	ft.closeOnce.Do(func() {
		close(ft.done)
		ft.mu.Lock()
		defer ft.mu.Unlock()
		for _, f := range ft.pending {
			f.close()
		}
		for _, f := range ft.incoming {
			f.close()
		}
	})
}

func (f *ftFile) close() {
	if f != nil && f.file != nil {
		_ = f.file.Close()
		f.file = nil
	}
}

func (ft *FileTransfer) Stats() TransferStats {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.stats
}

func (ft *FileTransfer) Put(pattern string) error {
	if ft.server {
		return fmt.Errorf("PUT is a client command")
	}
	sources, err := ftSources(pattern, ft.cwd())
	if len(sources) == 0 {
		if err != nil {
			return err
		}
		return os.ErrNotExist
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if err != nil {
		ft.stats.Errors++
		ft.note(fmt.Sprintf("put: %v", err))
	}
	for _, src := range sources {
		id := ft.newID()
		name := ftRelativeName(src.wire)
		flags := uint8(0)
		size := src.info.Size()
		if src.info.IsDir() {
			flags = ftFlagDir
			size = 0
		}
		pkt, err := ftPutPacket(id, ftModeBits(src.info.Mode()), size, uint32(src.info.ModTime().Unix()), flags, name)
		if err != nil {
			ft.stats.Errors++
			ft.note(fmt.Sprintf("put %s: %v", name, err))
			continue
		}
		ft.enqueue(pkt)
		if flags == 0 {
			ft.pending[id] = &ftFile{id: id, name: name, path: src.local, size: size,
				meta: ftMeta{src.info.Mode(), src.info.ModTime()}}
		}
	}
	return nil
}

func (ft *FileTransfer) Get(pattern string) error {
	if ft.server {
		return fmt.Errorf("GET is a client command")
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	id := ft.newID()
	pkt, err := ftListRequestPacket(id, pattern)
	if err != nil {
		return err
	}
	ft.requests[id] = ft.cwd()
	ft.enqueue(pkt)
	return nil
}

func (ft *FileTransfer) newID() uint32 {
	ft.nextID++
	if ft.nextID == 0 {
		ft.nextID++
	}
	return ft.nextID
}

// enqueue must be called with mu held.
func (ft *FileTransfer) enqueue(pkt ftPacket) {
	ft.queue = append(ft.queue, pkt)
	ft.signal()
}

func (ft *FileTransfer) signal() {
	select {
	case ft.wake <- struct{}{}:
	default:
	}
}

func (ft *FileTransfer) run() {
	for {
		select {
		case <-ft.done:
			return
		case <-ft.wake:
		}
		for {
			select {
			case <-ft.done:
				return
			default:
			}
			ft.mu.Lock()
			pkt, ok := ft.nextPacket()
			ft.mu.Unlock()
			if !ok {
				break
			}
			if err := ft.app.SendChannel(pkt.channel-chnOffset, pkt.payload); err != nil {
				ft.note(fmt.Sprintf("file transfer send: %v", err))
				return
			}
		}
	}
}

// nextPacket advances the sender state. It must be called with mu held.
func (ft *FileTransfer) nextPacket() (ftPacket, bool) {
	if len(ft.queue) > 0 {
		pkt := ft.queue[0]
		ft.queue[0] = ftPacket{}
		ft.queue = ft.queue[1:]
		return pkt, true
	}
	if ft.activeOut == nil && len(ft.ready) > 0 {
		f := ft.ready[0]
		ft.ready[0] = nil
		ft.ready = ft.ready[1:]
		if ft.pending[f.id] != f {
			return ft.nextPacket()
		}
		file, err := os.Open(f.path)
		if err != nil {
			return ft.sourceError(f, err), true
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			file.Close()
			return ft.sourceError(f, fmt.Errorf("source is not a regular file")), true
		}
		if info.Size() != f.size {
			file.Close()
			return ft.sourceError(f, fmt.Errorf("source size changed")), true
		}
		if f.offset == f.size && f.size != 0 {
			file.Close()
			if !ft.server {
				ft.finish(f, nil)
			}
			delete(ft.pending, f.id)
			return ftErrorPacket(f.id, ftErrNoData, ""), true
		}
		if f.offset < 0 || f.offset > f.size {
			f.offset = 0
		}
		if _, err := file.Seek(f.offset, io.SeekStart); err != nil {
			file.Close()
			return ft.sourceError(f, err), true
		}
		f.file, f.position, f.started = file, f.offset, time.Now()
		if f.size > f.offset {
			ft.activeOut = f
		} else {
			f.close()
			if ft.server {
				delete(ft.pending, f.id)
			}
		}
		return ftOffsetPacket(chnFTSwitch, f.id, f.offset), true
	}
	if f := ft.activeOut; f != nil {
		buf := make([]byte, ftMaxPayload)
		n, err := f.file.Read(buf)
		if n == 0 {
			ft.activeOut = nil
			return ft.sourceError(f, fmt.Errorf("read source: %w", err)), true
		}
		f.position += int64(n)
		f.transferred += int64(n)
		ft.progress(f)
		if f.position >= f.size {
			f.close()
			ft.activeOut = nil
			if ft.server {
				delete(ft.pending, f.id)
			}
		}
		return ftPacket{chnFTData, buf[:n]}, true
	}
	return ftPacket{}, false
}

func (ft *FileTransfer) sourceError(f *ftFile, err error) ftPacket {
	f.close()
	delete(ft.pending, f.id)
	if ft.activeOut == f {
		ft.activeOut = nil
	}
	ft.finish(f, err)
	return ftErrorPacket(f.id, ftErrBadFile, "")
}

func (ft *FileTransfer) note(message string) {
	if ft.report != nil {
		ft.report(message)
	}
}

func (ft *FileTransfer) progress(f *ftFile) {
	if time.Since(f.lastReport) < 300*time.Millisecond || f.size <= 0 {
		return
	}
	f.lastReport = time.Now()
	ft.note(fmt.Sprintf("%s %d%%", f.name, 100*f.position/f.size))
}

func (ft *FileTransfer) finish(f *ftFile, err error) {
	if err != nil {
		ft.stats.Errors++
		ft.note(fmt.Sprintf("%s: %v", f.name, err))
		return
	}
	ft.stats.Success++
	ft.stats.Bytes += f.transferred
	if !f.started.IsZero() {
		ft.stats.Elapsed += time.Since(f.started)
	}
	speed := int64(0)
	if !f.started.IsZero() && time.Since(f.started) > 0 {
		speed = int64(float64(f.transferred) / time.Since(f.started).Seconds())
	}
	ft.note(fmt.Sprintf("%s complete (%s/s; %d ok, %d failed)", f.name, formatSize(speed), ft.stats.Success, ft.stats.Errors))
}

func ftErrorCode(err error) uint8 {
	if errors.Is(err, os.ErrNotExist) {
		return ftErrNoEnt
	}
	if errors.Is(err, os.ErrPermission) {
		return ftErrPerm
	}
	return ftErrBadFile
}

func ftApplyMeta(path string, meta ftMeta) {
	_ = os.Chmod(path, meta.mode)
	if meta.mtime.Unix() != 0 {
		_ = os.Chtimes(path, meta.mtime, meta.mtime)
	}
}

func (ft *FileTransfer) restoreDirs(path string) {
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		if meta, ok := ft.dirs[parent]; ok {
			ftApplyMeta(parent, meta)
		}
		if parent == filepath.Dir(parent) {
			break
		}
	}
}

func ftUint64(data []byte) int64 { return int64(binary.BigEndian.Uint64(data)) }
