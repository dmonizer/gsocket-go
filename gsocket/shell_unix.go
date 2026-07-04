//go:build !windows

package gsocket

import "os"

// shellExec returns the shell to use for -e command execution.
// Uses $SHELL from environment, falling back to /bin/sh.
func shellExec() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	return "/bin/sh"
}

// shellInteractive returns the shell to use for -i interactive mode.
// Uses $SHELL from environment, falling back to /bin/sh.
func shellInteractive() string {
	return shellExec()
}

// shellExecArgs returns the arguments to pass to the shell for -e mode.
func shellExecArgs(command string) []string {
	return []string{"-c", command}
}

// shellInteractiveArgs returns the arguments for an interactive shell.
func shellInteractiveArgs() []string {
	return []string{"-i"}
}
