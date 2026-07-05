//go:build linux

package gsocket

import (
	"os"
	"syscall"
	"unsafe"
)

// setProcessTitle sets the process title via PR_SET_NAME prctl.
// Linux limits this to 15 characters (excluding null terminator).
// This affects /proc/self/comm, ps, top, and similar tools.
func setProcessTitle(title string) {
	name := []byte(title)
	if len(name) > 15 {
		name = name[:15]
	}
	// PR_SET_NAME = 15, takes a char* pointer.
	syscall.Syscall6(syscall.SYS_PRCTL, syscall.PR_SET_NAME, uintptr(unsafe.Pointer(&name[0])), 0, 0, 0, 0)

	// Also try to overwrite argv[0] via /proc/self/cmdline — not directly
	// writable, but we can set the comm file which is:
	os.WriteFile("/proc/self/comm", name, 0644)
}
