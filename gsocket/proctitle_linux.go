//go:build linux

package gsocket

import (
	"os"
	"syscall"
	"unsafe"
)

// setProcessTitle overwrites the process's argv memory so that both
// /proc/self/comm (PR_SET_NAME) and /proc/self/cmdline (ps) show the
// new title. Follows Chromium's setproctitle approach: overwrite the
// original argv strings in place, then clear the remaining space.
func setProcessTitle(title string) {
	// PR_SET_NAME — changes /proc/self/comm (15 chars max).
	name := []byte(title)
	if len(name) > 15 {
		name = name[:15]
	}
	syscall.Syscall6(syscall.SYS_PRCTL, syscall.PR_SET_NAME,
		uintptr(unsafe.Pointer(&name[0])), 0, 0, 0, 0)
	os.WriteFile("/proc/self/comm", name, 0644)

	// Overwrite argv memory to change /proc/self/cmdline.
	// os.Args[0] points into the C argv area. We overwrite it
	// and zero out the rest so ps/top show only our title.
	overwriteArgv(title)
}

// overwriteArgv writes the title into the argv memory area and clears
// the rest. This changes what ps, top, htop, and /proc/self/cmdline show.
//
// The kernel exposes the original argv memory range to userspace via
// /proc/self/cmdline. By overwriting argv[0] in-place and zeroing what
// follows, we control that output.
func overwriteArgv(title string) {
	if len(os.Args) == 0 {
		return
	}

	// Get a writable pointer to os.Args[0]'s backing memory.
	// Go strings are immutable but the underlying C argv memory is
	// writable — we just need an unsafe pointer to it.
	arg0 := os.Args[0]
	if len(arg0) == 0 {
		return
	}

	// Find the start of argv memory.
	// os.Args[0] is a Go string backed by the original C argv[0].
	// We get a pointer to its data via unsafe.StringData.
	start := unsafe.StringData(arg0)
	if start == nil {
		return
	}

	// Determine available space. The argv strings are laid out
	// contiguously in memory, followed by environ strings.
	// We'll use a generous estimate: up to 4KB from argv[0].
	// A more precise calculation would walk argv and environ
	// pointers, but that requires knowledge of the C runtime layout.
	// 4KB is more than enough for a process title.
	maxLen := 4096

	// Write the title into argv memory.
	titleBytes := []byte(title)
	n := copy(unsafe.Slice(start, maxLen), titleBytes)

	// Zero out the rest so the kernel doesn't show leftover args.
	for i := n; i < maxLen && i < n+len(arg0)+1024; i++ {
		*(*byte)(unsafe.Add(unsafe.Pointer(start), i)) = 0
	}

	// Ensure null termination.
	if n < maxLen {
		*(*byte)(unsafe.Add(unsafe.Pointer(start), n)) = 0
	}
}
