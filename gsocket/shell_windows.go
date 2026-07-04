//go:build windows

package gsocket

import (
	"os/exec"
	"syscall"
)

// shellExec returns the shell to use for -e command execution on Windows.
// Tries PowerShell first (more capable), falls back to cmd.exe.
func shellExec() string {
	if p, err := exec.LookPath("powershell.exe"); err == nil {
		return p
	}
	if p, err := exec.LookPath("cmd.exe"); err == nil {
		return p
	}
	return "cmd.exe"
}

// shellInteractive returns the shell for -i interactive mode on Windows.
// cmd.exe works better with pipes than powershell for interactive use,
// so prefer it when no explicit preference is set.
func shellInteractive() string {
	return shellExec()
}

// shellExecArgs returns arguments for -e mode (execute a command).
func shellExecArgs(command string) []string {
	shell := shellExec()
	if len(shell) >= 14 && shell[len(shell)-14:] == "powershell.exe" {
		return []string{"-NoProfile", "-NonInteractive", "-Command", command}
	}
	return []string{"/C", command}
}

// shellInteractiveArgs returns arguments for an interactive shell session
// using pipes (no real console). cmd.exe /Q keeps it quiet.
func shellInteractiveArgs() []string {
	shell := shellInteractive()
	if len(shell) >= 14 && shell[len(shell)-14:] == "powershell.exe" {
		// -NoLogo: suppress banner, -NoExit: stay alive, -Command -: read from stdin
		return []string{"-NoLogo", "-NoExit", "-Command", "-"}
	}
	// cmd.exe: /Q turns echo off, stdin/stdout work via pipes.
	return []string{"/Q"}
}

// setShellSysProcAttr prevents the child shell from writing directly to the
// parent's console. Without this, powershell/cmd can use WriteConsole to
// bypass the stdout pipe, sending output to the server console instead of
// the GS channel. CREATE_NO_WINDOW forces pipe-only I/O.
func setShellSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}
