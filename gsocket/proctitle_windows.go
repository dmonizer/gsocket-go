//go:build windows

package gsocket

import (
	"syscall"
	"unsafe"
)

var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleTitleW = kernel32.NewProc("SetConsoleTitleW")
)

func setProcessTitle(title string) {
	// SetConsoleTitle changes the console window title bar text.
	// This is visible in the taskbar and window title, though not
	// in Task Manager's process list (which shows the exe name).
	ptr, _ := syscall.UTF16PtrFromString(title)
	procSetConsoleTitleW.Call(uintptr(unsafe.Pointer(ptr)))
}
