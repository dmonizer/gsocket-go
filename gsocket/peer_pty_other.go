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
func (p *Peer) runWithPTY(shell string) error {
	cmd := exec.Command(shell, "-i")
	cmd.Stderr = cmd.Stdout

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
