//go:build !linux && !windows

package gsocket

// setProcessTitle is a no-op on this platform.
func setProcessTitle(title string) {}
