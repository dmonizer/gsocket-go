//go:build windows

package gsocket

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// ConPTY constants — matches Windows SDK definitions.
const (
	// PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE is the attribute type for
	// attaching a ConPTY handle to a child process in STARTUPINFOEX.
	_PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE = 0x00020016

	// EXTENDED_STARTUPINFO_PRESENT tells CreateProcessW to read the
	// extended STARTUPINFOEX fields (including the attribute list).
	_EXTENDED_STARTUPINFO_PRESENT = 0x00080000

	// Default pseudo console size.
	_defaultConPTYCols = 80
	_defaultConPTYRows = 24
)

// coord matches the Windows COORD struct.
type _coord struct {
	X int16
	Y int16
}

// --- ConPTY kernel32 wrappers ---

var (
	_kernel32                = syscall.NewLazyDLL("kernel32.dll")
	_procCreatePseudoConsole = _kernel32.NewProc("CreatePseudoConsole")
	_procClosePseudoConsole  = _kernel32.NewProc("ClosePseudoConsole")
	_procResizePseudoConsole = _kernel32.NewProc("ResizePseudoConsole")
	_procCreateProcessW      = _kernel32.NewProc("CreateProcessW")

	_procInitializeProcThreadAttributeList = _kernel32.NewProc("InitializeProcThreadAttributeList")
	_procUpdateProcThreadAttribute         = _kernel32.NewProc("UpdateProcThreadAttribute")
	_procDeleteProcThreadAttributeList     = _kernel32.NewProc("DeleteProcThreadAttributeList")

	// Job Object API — used to ensure all child processes are killed
	// when the shell is terminated (cmd.exe→powershell.exe chain).
	_procCreateJobObject          = _kernel32.NewProc("CreateJobObjectW")
	_procSetInformationJobObject  = _kernel32.NewProc("SetInformationJobObject")
	_procAssignProcessToJobObject = _kernel32.NewProc("AssignProcessToJobObject")
	_procOpenProcess              = _kernel32.NewProc("OpenProcess")
)

// Job Object constants.
const (
	_PROCESS_SET_QUOTA                     = 0x0100
	_PROCESS_TERMINATE                     = 0x0001
	_JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE    = 0x2000
	_JOB_OBJECT_EXTENDED_LIMIT_INFORMATION = 9
)

type _jobObjectExtendedLimitInfo struct {
	BasicLimitInformation struct {
		PerProcessUserTimeLimit uint64
		PerJobUserTimeLimit     uint64
		LimitFlags              uint32
		MinimumWorkingSetSize   uintptr
		MaximumWorkingSetSize   uintptr
		ActiveProcessLimit      uint32
		Affinity                uintptr
		PriorityClass           uint32
		SchedulingClass         uint32
	}
	IoInfo struct {
		ReadOperationCount  uint64
		WriteOperationCount uint64
		OtherOperationCount uint64
		ReadTransferCount   uint64
		WriteTransferCount  uint64
		OtherTransferCount  uint64
	}
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// createKillOnCloseJob creates a Windows Job Object configured to
// kill all assigned processes when the job handle is closed.
func createKillOnCloseJob() (uintptr, error) {
	job, _, err := _procCreateJobObject.Call(0, 0)
	if job == 0 {
		return 0, fmt.Errorf("CreateJobObject failed: %w", err)
	}

	var info _jobObjectExtendedLimitInfo
	info.BasicLimitInformation.LimitFlags = _JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE

	r1, _, err := _procSetInformationJobObject.Call(
		job,
		_JOB_OBJECT_EXTENDED_LIMIT_INFORMATION,
		uintptr(unsafe.Pointer(&info)),
		uintptr(unsafe.Sizeof(info)),
	)
	if r1 == 0 {
		syscall.CloseHandle(syscall.Handle(job))
		return 0, fmt.Errorf("SetInformationJobObject failed: %w", err)
	}

	return job, nil
}

// assignProcessToJob adds a process (by PID) to a job object.
func assignProcessToJob(job uintptr, pid int) error {
	proc, _, err := _procOpenProcess.Call(
		uintptr(_PROCESS_SET_QUOTA|_PROCESS_TERMINATE),
		0,
		uintptr(pid),
	)
	if proc == 0 {
		return fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	defer syscall.CloseHandle(syscall.Handle(proc))

	r1, _, err := _procAssignProcessToJobObject.Call(job, proc)
	if r1 == 0 {
		return fmt.Errorf("AssignProcessToJobObject(%d): %w", pid, err)
	}
	return nil
}

// createPseudoConsole creates a new Windows Pseudo Console.
//
//	size: initial terminal dimensions
//	input: readable handle for ConPTY input (our stdin pipe read end)
//	output: writable handle for ConPTY output (our stdout pipe write end)
//
// Returns the HPCON handle on success. On pre-1809 Windows where
// CreatePseudoConsole doesn't exist in kernel32.dll, the underlying
// LazyProc.Call panics — we recover and return it as an error so the
// caller can fall back to pipes.
func createPseudoConsole(size _coord, input, output syscall.Handle) (h syscall.Handle, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("CreatePseudoConsole not available (requires Windows 10 1809+): %v", r)
		}
	}()
	var conpty syscall.Handle
	r1, _, callErr := _procCreatePseudoConsole.Call(
		uintptr(unsafe.Pointer(&size)),
		uintptr(input),
		uintptr(output),
		0, // dwFlags — must be 0
		uintptr(unsafe.Pointer(&conpty)),
	)
	// CreatePseudoConsole returns S_OK (0) on success.
	if r1 != 0 {
		return 0, fmt.Errorf("CreatePseudoConsole failed: %w", callErr)
	}
	return conpty, nil
}

// closePseudoConsole closes a Pseudo Console handle.
func closePseudoConsole(h syscall.Handle) {
	_procClosePseudoConsole.Call(uintptr(h))
}

// resizePseudoConsole resizes the pseudo console to new dimensions.
func resizePseudoConsole(h syscall.Handle, size _coord) error {
	r1, _, err := _procResizePseudoConsole.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(&size)),
	)
	if r1 != 0 {
		return fmt.Errorf("ResizePseudoConsole failed: %w", err)
	}
	return nil
}

// --- Process creation with ConPTY attribute ---

// startConPTYProcess creates a child process attached to the given
// pseudo console. Uses CreateProcessW with STARTUPINFOEX to pass the
// PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE attribute.
func startConPTYProcess(shell string, args []string, conpty syscall.Handle) (*os.Process, error) {
	// Build the command line. On Windows, CreateProcessW can take NULL
	// for lpApplicationName when the full path is in lpCommandLine.
	cmdLine, err := syscall.UTF16PtrFromString(windowsCmdLine(shell, args))
	if err != nil {
		return nil, fmt.Errorf("build command line: %w", err)
	}

	// --- Build STARTUPINFOEX with attribute list ---
	//
	// We need to wrap the ConPTY handle in a process attribute list.
	// Per MSDN:
	//   1. Call InitializeProcThreadAttributeList twice —
	//      first to get the required size, second to initialize.
	//   2. Call UpdateProcThreadAttribute to set the ConPTY handle.
	//   3. Pass the list in STARTUPINFOEX.lpAttributeList.
	//   4. After CreateProcess, call DeleteProcThreadAttributeList.

	// Step 1: determine the required attribute list size.
	var listSize uintptr
	_procInitializeProcThreadAttributeList.Call(
		0, // lpAttributeList = NULL → return required size
		1, // dwAttributeCount
		0, // dwFlags
		uintptr(unsafe.Pointer(&listSize)),
	)

	// Allocate the attribute list buffer.
	attrList := make([]byte, listSize)

	// Step 2: initialize the attribute list.
	r1, _, err := _procInitializeProcThreadAttributeList.Call(
		uintptr(unsafe.Pointer(&attrList[0])),
		1, // dwAttributeCount
		0, // dwFlags
		uintptr(unsafe.Pointer(&listSize)),
	)
	if r1 == 0 {
		return nil, fmt.Errorf("InitializeProcThreadAttributeList failed: %w", err)
	}

	// Step 3: add the ConPTY handle as a pseudo console attribute.
	r1, _, err = _procUpdateProcThreadAttribute.Call(
		uintptr(unsafe.Pointer(&attrList[0])),
		0, // dwFlags
		_PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		uintptr(conpty),
		unsafe.Sizeof(conpty),
		0, // lpPreviousValue
		0, // lpReturnSize
	)
	if r1 == 0 {
		_procDeleteProcThreadAttributeList.Call(uintptr(unsafe.Pointer(&attrList[0])))
		return nil, fmt.Errorf("UpdateProcThreadAttribute failed: %w", err)
	}

	// Build STARTUPINFOEX. We use a properly sized struct that embeds
	// the standard STARTUPINFOW fields followed by the attribute list pointer.
	type startupInfoEx struct {
		cb              uint32
		lpReserved      uintptr
		lpDesktop       uintptr
		lpTitle         uintptr
		dwX             uint32
		dwY             uint32
		dwXSize         uint32
		dwYSize         uint32
		dwXCountChars   uint32
		dwYCountChars   uint32
		dwFillAttribute uint32
		dwFlags         uint32
		wShowWindow     uint16
		cbReserved2     uint16
		lpReserved2     uintptr
		hStdInput       syscall.Handle
		hStdOutput      syscall.Handle
		hStdError       syscall.Handle
		lpAttributeList uintptr
	}
	siEx := startupInfoEx{
		lpAttributeList: uintptr(unsafe.Pointer(&attrList[0])),
	}
	siEx.cb = uint32(unsafe.Sizeof(siEx))

	var procInfo struct {
		process   syscall.Handle
		thread    syscall.Handle
		processID uint32
		threadID  uint32
	}

	// Step 4: create the process with EXTENDED_STARTUPINFO_PRESENT.
	r1, _, err = _procCreateProcessW.Call(
		0,                                      // lpApplicationName (NULL)
		uintptr(unsafe.Pointer(cmdLine)),       // lpCommandLine
		0,                                      // lpProcessAttributes
		0,                                      // lpThreadAttributes
		1,                                      // bInheritHandles
		uintptr(_EXTENDED_STARTUPINFO_PRESENT), // dwCreationFlags
		0,                                      // lpEnvironment
		0,                                      // lpCurrentDirectory
		uintptr(unsafe.Pointer(&siEx)),         // lpStartupInfo
		uintptr(unsafe.Pointer(&procInfo)),     // lpProcessInformation
	)

	// Clean up the attribute list regardless of outcome.
	_procDeleteProcThreadAttributeList.Call(uintptr(unsafe.Pointer(&attrList[0])))

	if r1 == 0 {
		return nil, fmt.Errorf("CreateProcessW failed: %w", err)
	}

	// Close the thread handle — we only need the process handle.
	syscall.CloseHandle(procInfo.thread)

	proc, procErr := os.FindProcess(int(procInfo.processID))
	if procErr != nil {
		return nil, fmt.Errorf("FindProcess: %w", procErr)
	}
	return proc, nil
}

// windowsCmdLine builds the Windows command line string from the
// executable path and arguments.
func windowsCmdLine(exe string, args []string) string {
	var b strings.Builder
	b.WriteString(syscall.EscapeArg(exe))
	for _, a := range args {
		b.WriteByte(' ')
		b.WriteString(syscall.EscapeArg(a))
	}
	return b.String()
}

// --- runWithPTY: Windows entry point ---

// runWithPipes is the fallback interactive shell using plain pipes.
// Used when ConPTY is not requested or not available.
func (p *Peer) runWithPipes(shell string) error {
	p.hasPTY = false

	// Notify client that we don't have a PTY.
	status := []byte{StatusTypeNoPTY}
	if serr := p.app.SendMessage(msgStatus, status); serr != nil {
		p.logger.Printf("Failed to send NOPTY status: %v", serr)
	}

	cmd := exec.Command(shell, shellInteractiveArgs()...)
	setShellSysProcAttr(cmd)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start shell: %w", err)
	}

	// Wrap the shell in a job object so that when we kill it, all
	// child processes (e.g. powershell.exe spawned from cmd.exe)
	// are killed too. On cleanup we close the job handle, which
	// triggers JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE.
	var job uintptr
	if j, err := createKillOnCloseJob(); err == nil {
		job = j
		assignProcessToJob(job, cmd.Process.Pid)
	} // else: fall back to Process.Kill without job object

	// Kill shell when peer is closed.
	go func() {
		<-p.done
		killProcessTree(cmd.Process, job)
	}()

	// Channel → Shell stdin.
	go func() {
		defer stdinPipe.Close()
		defer func() {
			killProcessTree(cmd.Process, job)
		}()
		defer p.Close()
		buf := make([]byte, 8192)
		for {
			n, err := p.channel.Read(buf)
			if n > 0 {
				plaintext, derr := p.app.Decode(buf[:n])
				if derr != nil {
					return
				}
				// Without a PTY there is no line discipline to handle
				// Ctrl-C or translate \r→\n. Do both here.
				translated := make([]byte, 0, len(plaintext))
				for _, b := range plaintext {
					if b == 0x03 {
						p.logger.Printf("Ctrl-C received — killing shell")
						killProcessTree(cmd.Process, job)
						return
					}
					if b == '\r' {
						translated = append(translated, '\n')
					} else {
						translated = append(translated, b)
					}
				}
				if len(translated) > 0 {
					if _, werr := stdinPipe.Write(translated); werr != nil {
						return
					}
				}
				p.mu.Lock()
				p.bytesRead += int64(n)
				p.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	// Shell stdout → Channel.
	io.Copy(p.app, stdoutPipe)
	killProcessTree(cmd.Process, job)
	p.Close()
	cmd.Wait()
	return nil
}

// killProcessTree kills the process and all its children. If a job
// object was created, closing the job handle kills the entire tree
// atomically. Falls back to os.Process.Kill() if no job is set.
func killProcessTree(proc *os.Process, job uintptr) {
	if job != 0 {
		syscall.CloseHandle(syscall.Handle(job))
		return
	}
	if proc != nil {
		proc.Kill()
	}
}

// runWithPTY spawns an interactive shell. On Windows with -conpty,
// tries the Pseudo Console API for proper terminal semantics. Falls
// back to pipe-based I/O if ConPTY fails (e.g. pre-1809 Windows).
func (p *Peer) runWithPTY(shell string) error {
	if !p.useConPTY {
		return p.runWithPipes(shell)
	}

	err := p.runWithConPTY(shell)
	if err != nil {
		// ConPTY failed — likely pre-Windows 10 1809. Fall back to
		// plain pipes so the session still works.
		p.logger.Printf("ConPTY failed: %v — falling back to pipes", err)
		return p.runWithPipes(shell)
	}
	return nil
}

// --- ConPTY-based interactive shell ---

// runWithConPTY spawns the interactive shell using Windows ConPTY.
// The child gets a proper console (echo, prompts, Ctrl-C handling,
// resize support) matching Linux PTY behavior.
func (p *Peer) runWithConPTY(shell string) error {
	// Create two pipe pairs for communication with the pseudo console.
	//
	//    coninR ← coninW   (we write to coninW, ConPTY reads from coninR)
	//    conoutR ← conoutW (ConPTY writes to conoutW, we read from conoutR)
	//
	coninR, coninW, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("conin pipe: %w", err)
	}
	defer coninR.Close()

	conoutR, conoutW, err := os.Pipe()
	if err != nil {
		coninW.Close()
		return fmt.Errorf("conout pipe: %w", err)
	}
	defer conoutW.Close()

	size := _coord{X: _defaultConPTYCols, Y: _defaultConPTYRows}

	conpty, err := createPseudoConsole(
		size,
		syscall.Handle(coninR.Fd()),
		syscall.Handle(conoutW.Fd()),
	)
	if err != nil {
		coninW.Close()
		conoutR.Close()
		return fmt.Errorf("create pseudo console: %w", err)
	}
	defer closePseudoConsole(conpty)

	// Store ConPTY handle for resize callbacks.
	p.mu.Lock()
	p.ptyMasterFd = uintptr(conpty)
	p.hasPTY = true
	p.mu.Unlock()

	// Start the shell process attached to the pseudo console.
	// NOTE: we do NOT call setShellSysProcAttr — ConPTY handles
	// terminal isolation; CREATE_NO_WINDOW would conflict.
	proc, err := startConPTYProcess(shell, shellInteractiveArgs(), conpty)
	if err != nil {
		return fmt.Errorf("start shell: %w", err)
	}

	// Wrap in a job object so grandchildren are killed on cleanup.
	var job uintptr
	if j, err := createKillOnCloseJob(); err == nil {
		job = j
		assignProcessToJob(job, proc.Pid)
	}

	// Kill shell tree when peer is closed.
	go func() {
		<-p.done
		killProcessTree(proc, job)
	}()

	var wg sync.WaitGroup
	wg.Add(1)

	// ConPTY output → Channel (goroutine).
	//
	// Must run concurrently with the input goroutine — ConPTY has an
	// internal thread servicing the output pipe. If we don't drain the
	// output, the pseudo console deadlocks.
	go func() {
		defer wg.Done()
		defer p.Close()
		defer conoutR.Close()
		defer coninW.Close() // signal ConPTY no more input

		buf := make([]byte, 8192)
		for {
			n, err := conoutR.Read(buf)
			if n > 0 {
				if _, werr := p.app.WriteData(buf[:n]); werr != nil {
					return
				}
				p.mu.Lock()
				p.bytesRead += int64(n)
				p.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	// Channel → ConPTY input (main goroutine, blocks until channel closes).
	//
	// ConPTY provides proper line discipline (unlike pipes), so we
	// write plaintext directly — no \r→\n translation needed.
	buf := make([]byte, 8192)
	for {
		n, err := p.channel.Read(buf)
		if n > 0 {
			plaintext, derr := p.app.Decode(buf[:n])
			if derr != nil {
				break
			}
			if len(plaintext) > 0 {
				if _, werr := coninW.Write(plaintext); werr != nil {
					break
				}
			}
			p.mu.Lock()
			p.bytesWritten += int64(n)
			p.mu.Unlock()
		}
		if err != nil {
			break
		}
	}

	// Signal the output goroutine that we're done.
	coninW.Close()
	conoutR.Close()

	killProcessTree(proc, job)
	wg.Wait()
	p.Close()
	return nil
}

// registerWinchHandler is a no-op on Windows. There is no SIGWINCH
// signal, but the WSIZE app message from the client still arrives and
// calls resizePTY, which delegates to resizePseudoConsole via the
// ConPTY handle stored in p.ptyMasterFd.
func (p *Peer) registerWinchHandler() {}

// resizePTY resizes the ConPTY pseudo console. Called when the client
// sends a WSIZE application message. On Windows without ConPTY this is
// a no-op (pipes don't support resize).
func resizePTY(hPCON uintptr, rows, cols uint16) error {
	if hPCON == 0 {
		return nil // not using ConPTY
	}
	return resizePseudoConsole(syscall.Handle(hPCON), _coord{X: int16(cols), Y: int16(rows)})
}
