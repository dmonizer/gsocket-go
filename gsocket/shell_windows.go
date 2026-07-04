//go:build windows

package gsocket

import "os/exec"

// shellExec returns the shell to use for -e command execution on Windows.
// Tries PowerShell first (more capable), falls back to cmd.exe.
func shellExec() string {
	// Try PowerShell first — it handles quoting and Unix-style syntax better.
	if p, err := exec.LookPath("powershell.exe"); err == nil {
		return p
	}
	// Fall back to cmd.exe — always available on Windows.
	if p, err := exec.LookPath("cmd.exe"); err == nil {
		return p
	}
	// Absolute fallback (should never happen on real Windows).
	return "cmd.exe"
}

// shellInteractive returns the shell for -i interactive mode on Windows.
func shellInteractive() string {
	return shellExec()
}

// shellExecArgs returns arguments for -e mode (execute a command).
func shellExecArgs(command string) []string {
	shell := shellExec()
	// Check if it's PowerShell by looking at the path.
	if len(shell) >= 14 && shell[len(shell)-14:] == "powershell.exe" {
		return []string{"-NoProfile", "-NonInteractive", "-Command", command}
	}
	// cmd.exe or fallback.
	return []string{"/C", command}
}

// shellInteractiveArgs returns arguments for an interactive shell session.
func shellInteractiveArgs() []string {
	shell := shellInteractive()
	if len(shell) >= 14 && shell[len(shell)-14:] == "powershell.exe" {
		return []string{"-NoLogo"}
	}
	// cmd.exe with no args gives an interactive prompt.
	return nil
}
