package gsocket

// SetProcessTitle attempts to change the process title on supported platforms.
// On Linux, this uses prctl(PR_SET_NAME) (15 chars max).
// On Windows, this sets the console window title.
// On macOS, this uses pthread_setname_np for the current thread.
// A best-effort function — failures are silent.
func SetProcessTitle(title string) {
	setProcessTitle(title)
}
