//go:build linux

package gsocket

import (
	"fmt"
	"io"
	"os"
	"os/exec"
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

// runWithPTY spawns the given shell in a new PTY and relays data between
// the PTY master and the encrypted channel.
func (p *Peer) runWithPTY(shell string) error {
	ptyMaster, ptySlave, err := openPTY()
	if err != nil {
		return fmt.Errorf("allocate PTY: %w", err)
	}
	defer ptyMaster.Close()
	defer ptySlave.Close()

	cmd := exec.Command(shell, "-i")
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
