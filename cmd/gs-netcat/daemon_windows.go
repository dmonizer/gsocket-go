//go:build windows

package main

import (
	"log"
	"os/exec"
)

// reexecAsDaemon is a no-op on Windows — daemon mode is not supported.
func reexecAsDaemon() {
	log.Fatal("daemon mode (-D) is not supported on Windows")
}

// detachFromTerminal is a no-op on Windows.
func detachFromTerminal() {}

// getExitCode extracts the exit code from a child process error on Windows.
func getExitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return 255
}
