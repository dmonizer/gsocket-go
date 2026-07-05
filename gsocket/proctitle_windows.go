//go:build windows

package gsocket

// setProcessTitle is a no-op on Windows. Task Manager shows the
// executable filename from the kernel's EPROCESS structure, which
// userspace cannot change. Rename the binary for a custom name.
func setProcessTitle(title string) {}
