//go:build !linux

package gsocket

import (
	"fmt"
	"io"
	"os/exec"
)

// runWithPTY spawns an interactive shell using pipes as a fallback when
// PTY allocation is not available (non-Linux platforms).
//
// Without a PTY the shell session lacks job control and Ctrl-C may kill
// gs-netcat rather than foreground tasks inside the shell. For a proper
// interactive experience use Linux where a PTY is allocated.
//
// Sends a NOPTY status message to the client before relaying begins,
// matching C's pkt_app_send_status_nopty().
func (p *Peer) runWithPTY(shell string) error {
	p.hasPTY = false

	// Notify client that we don't have a PTY — client should not expect
	// WSIZE messages or PTY-specific behavior.
	status := []byte{StatusTypeNoPTY}
	if serr := p.app.SendMessage(msgStatus, status); serr != nil {
		p.logger.Printf("Failed to send NOPTY status: %v", serr)
	}

	cmd := exec.Command(shell, shellInteractiveArgs()...)
	setShellSysProcAttr(cmd)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	// Merge stderr into stdout AFTER StdoutPipe() creates the pipe.
	// If set before, cmd.Stdout is nil and stderr defaults to os.Stderr.
	cmd.Stderr = cmd.Stdout

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

// registerWinchHandler is a no-op on non-Linux platforms — there is no
// SIGWINCH or PTY to resize. PTY allocation is not available on these
// platforms (see runWithPTY which uses pipes as fallback).
func (p *Peer) registerWinchHandler() {}

// resizePTY is a no-op on non-Linux — PTY allocation is not available.
func resizePTY(masterFd uintptr, rows, cols uint16) error { return nil }
