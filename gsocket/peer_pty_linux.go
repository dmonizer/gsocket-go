//go:build linux

package gsocket

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"unsafe"
)

// openPTY opens a new PTY master/slave pair. The master is used by the
// relay to read/write data; the slave is attached to the child process
// as its controlling terminal.
func openPTY() (master, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	defer func() {
		if err != nil {
			m.Close()
		}
	}()

	// Get the PTY number from the kernel.
	var n uint32
	_, _, eno := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n)))
	if eno != 0 {
		return nil, nil, fmt.Errorf("TIOCGPTN: %v", eno)
	}

	// Unlock the slave side so it can be opened.
	var unlock int32 // zero = unlock
	_, _, eno = syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock)))
	if eno != 0 {
		return nil, nil, fmt.Errorf("TIOCSPTLCK: %v", eno)
	}

	sname := fmt.Sprintf("/dev/pts/%d", n)

	// Use syscall.Open — NOT os.OpenFile — because Go's os.OpenFile
	// always sets O_CLOEXEC, which would close the slave fd when the
	// child execs the shell. The child must inherit this fd as its
	// controlling terminal.
	slaveFd, err := syscall.Open(sname, syscall.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", sname, err)
	}
	s := os.NewFile(uintptr(slaveFd), sname)
	if s == nil {
		syscall.Close(slaveFd)
		return nil, nil, fmt.Errorf("NewFile for %s failed", sname)
	}

	return m, s, nil
}

// resizePTY resizes the PTY master to the given terminal dimensions.
// Matches the C TIOCSWINSZ ioctl in pkt_app_cb_wsize().
func resizePTY(masterFd uintptr, rows, cols uint16) error {
	ws := struct {
		row    uint16
		col    uint16
		xpixel uint16
		ypixel uint16
	}{
		row: rows,
		col: cols,
	}
	_, _, eno := syscall.Syscall(
		syscall.SYS_IOCTL,
		masterFd,
		syscall.TIOCSWINSZ,
		uintptr(unsafe.Pointer(&ws)),
	)
	if eno != 0 {
		return fmt.Errorf("TIOCSWINSZ: %v", eno)
	}
	return nil
}

// getTerminalSize returns the current terminal window size via TIOCGWINSZ.
// Used on the client side to detect window size changes.
func getTerminalSize(fd int) (rows, cols uint16, err error) {
	ws := struct {
		row    uint16
		col    uint16
		xpixel uint16
		ypixel uint16
	}{}
	_, _, eno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		syscall.TIOCGWINSZ,
		uintptr(unsafe.Pointer(&ws)),
	)
	if eno != 0 {
		return 0, 0, fmt.Errorf("TIOCGWINSZ: %v", eno)
	}
	return ws.row, ws.col, nil
}

// sendWSIZE builds and sends a WSIZE application message with the given
// terminal dimensions. Matches C's pkt_app_send_wsize().
func (p *Peer) sendWSIZE(rows, cols uint16) error {
	data := make([]byte, 4)
	binary.BigEndian.PutUint16(data[0:2], cols)
	binary.BigEndian.PutUint16(data[2:4], rows)
	return p.app.SendMessage(msgWSize, data)
}

// runWithPTY spawns the given shell in a new PTY and relays data between
// the PTY master and the encrypted channel.
func (p *Peer) runWithPTY(shell string) error {
	ptyMaster, ptySlave, err := openPTY()
	if err != nil {
		// PTY allocation failed — notify client and fall back to pipes.
		// Matches C's pkt_app_send_status_nopty().
		p.logger.Printf("PTY allocation failed: %v — falling back to pipes", err)
		p.hasPTY = false
		status := []byte{StatusTypeNoPTY}
		if serr := p.app.SendMessage(msgStatus, status); serr != nil {
			p.logger.Printf("Failed to send NOPTY status: %v", serr)
		}
		return p.runWithPipes(shell)
	}
	defer ptyMaster.Close()
	defer ptySlave.Close()

	// Store PTY master fd for WSIZE resize callbacks.
	p.mu.Lock()
	p.ptyMasterFd = ptyMaster.Fd()
	p.hasPTY = true
	p.mu.Unlock()

	cmd := exec.Command(shell, shellInteractiveArgs()...)
	cmd.Stdin = ptySlave
	cmd.Stdout = ptySlave
	cmd.Stderr = ptySlave

	// Start a new session: the child becomes session leader with the
	// PTY slave as its controlling terminal.
	// Ctty is 0 because the Go runtime dup2's Stdin onto fd 0 in the
	// child BEFORE calling TIOCSCTTY. Using fd 0 avoids any O_CLOEXEC
	// issues with the original slave fd.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start shell: %w", err)
	}

	// Close our copy of the slave fd — the child owns it now.
	ptySlave.Close()

	// Channel → PTY master (remote input → shell stdin).
	go func() {
		defer ptyMaster.Close()
		defer p.Close()
		buf := make([]byte, 8192)
		for {
			n, err := p.channel.Read(buf)
			if n > 0 {
				plaintext, derr := p.app.Decode(buf[:n])
				if derr != nil {
					return
				}
				if len(plaintext) > 0 {
					if _, werr := ptyMaster.Write(plaintext); werr != nil {
						return
					}
				}
				p.mu.Lock()
				p.bytesRead += int64(n)
				p.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	// PTY master → Channel (shell output → remote).
	_, err = io.Copy(p.channel, ptyMaster)
	p.Close()
	cmd.Wait()
	return nil
}

// runWithPipes is the fallback when PTY allocation fails. It uses plain
// pipes instead of a PTY, matching C's stty_switch_nopty() behavior.
func (p *Peer) runWithPipes(shell string) error {
	cmd := exec.Command(shell, shellInteractiveArgs()...)
	cmd.Stderr = cmd.Stdout
	setShellSysProcAttr(cmd)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start shell: %w", err)
	}

	// Channel → Shell stdin.
	go func() {
		defer stdinPipe.Close()
		defer p.Close()
		buf := make([]byte, 8192)
		for {
			n, err := p.channel.Read(buf)
			if n > 0 {
				plaintext, derr := p.app.Decode(buf[:n])
				if derr != nil {
					return
				}
				if len(plaintext) > 0 {
					if _, werr := stdinPipe.Write(plaintext); werr != nil {
						return
					}
				}
				p.mu.Lock()
				p.bytesRead += int64(n)
				p.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	// Shell stdout → Channel (blocks until shell exits).
	io.Copy(p.channel, stdoutPipe)
	p.Close()
	cmd.Wait()
	return nil
}

// registerWinchHandler sets up a SIGWINCH signal handler on the client side.
// When the terminal window is resized, the handler captures the new dimensions
// and sends a WSIZE application message to the server, which applies the
// resize to the PTY master via TIOCSWINSZ.
func (p *Peer) registerWinchHandler() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	go func() {
		for {
			select {
			case <-sigCh:
				rows, cols, err := getTerminalSize(int(os.Stdin.Fd()))
				if err != nil {
					continue
				}
				_ = p.sendWSIZE(rows, cols)
			case <-p.done:
				signal.Stop(sigCh)
				return
			}
		}
	}()
}
