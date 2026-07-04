//go:build !windows

package main

import (
	"log"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// reexecAsDaemon re-executes the current binary with _GSOCKET_DAEMON_CHILD=1
// set in the environment. The current (parent) process should exit immediately
// after this call returns. Matches C's GS_daemonize() first fork.
func reexecAsDaemon() {
	args := os.Args
	env := os.Environ()
	env = append(env, envDaemonChild+"=1")

	// Propagate _GSOCKET_WORKER so that -D -W daemonizes AND watchdog-wraps.
	if os.Getenv(envWorker) != "" {
		env = append(env, envWorker+"=1")
	}

	// Resolve full path — args[0] may be a bare filename.
	exePath := args[0]
	if ep, err := os.Executable(); err == nil {
		exePath = ep
	}

	cmd := exec.Command(exePath, args[1:]...)
	cmd.Env = env
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		log.Fatalf("daemon: re-exec failed: %v", err)
	}
	// Parent exits — the child is now the daemon.
}

// detachFromTerminal detaches the process from its controlling terminal.
// Called by the re-exec'd daemon child (when _GSOCKET_DAEMON_CHILD is set).
// Matches C's GS_daemonize(): setsid(), close stdin/stdout/stderr.
func detachFromTerminal() {
	// Start a new session — detach from the controlling terminal.
	if _, err := unix.Setsid(); err != nil {
		log.Printf("daemon: setsid() failed: %v", err)
	}

	// Chdir to root so we don't pin a mount point.
	os.Chdir("/")

	// Close standard file descriptors and redirect to /dev/null.
	null, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
	if err != nil {
		log.Printf("daemon: open /dev/null: %v", err)
		return
	}
	unix.Dup2(int(null.Fd()), unix.Stdin)
	unix.Dup2(int(null.Fd()), unix.Stdout)
	unix.Dup2(int(null.Fd()), unix.Stderr)
	if null.Fd() > 2 {
		null.Close()
	}
}

// getExitCode extracts the exit code from a child process error.
func getExitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			return ws.ExitStatus()
		}
	}
	return 255
}
