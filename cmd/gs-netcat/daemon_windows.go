//go:build windows

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	serviceName = "gs-netcat"
	serviceDesc = "Global Socket Relay Network — encrypted tunnel"
)

// serviceHandler implements svc.Handler for the Windows service.
type serviceHandler struct {
	workerDone chan struct{}
	workerErr  error
}

func (h *serviceHandler) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (ssec bool, errno uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}

	// Run the actual work in a goroutine — this is the normal gs-netcat
	// entry point (runListener or runClient called from serviceMain).
	go func() {
		defer close(h.workerDone)
		h.workerErr = realMain()
	}()

	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}

	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				// Trigger shutdown — same as Ctrl-C on console.
				// The realMain function handles SIGINT via its own signal handler.
				// We also wait for worker to finish.
				select {
				case <-h.workerDone:
				case <-time.After(15 * time.Second):
				}
				changes <- svc.Status{State: svc.Stopped}
				return
			default:
				log.Printf("service: unexpected control request #%d", c.Cmd)
			}
		case <-h.workerDone:
			// Worker exited on its own.
			changes <- svc.Status{State: svc.Stopped}
			return
		}
	}
}

// reexecAsDaemon installs and starts gs-netcat as a Windows service.
// The service will run with the same arguments.
func reexecAsDaemon() {
	exePath, err := os.Executable()
	if err != nil {
		log.Fatalf("daemon: cannot resolve executable path: %v", err)
	}

	// Build the service command line: exe + all args except -D.
	var svcArgs []string
	for _, a := range os.Args[1:] {
		if a != "-D" {
			svcArgs = append(svcArgs, a)
		}
	}

	// Connect to the Service Control Manager.
	m, err := mgr.Connect()
	if err != nil {
		log.Fatalf("daemon: cannot connect to Service Control Manager: %v\n"+
			"Run as Administrator to install a Windows service.", err)
	}
	defer m.Disconnect()

	// Build the full command line (binary + all args except -D).
	// This is stored as the service's binary path — no need to pass
	// separate start args to s.Start().
	cmd := fmt.Sprintf(`"%s" %s`, exePath, strings.Join(svcArgs, " "))

	// Check if service already exists.
	s, err := m.OpenService(serviceName)
	if err == nil {
		s.Close()
		fmt.Fprintf(os.Stderr, "%s: service '%s' already installed, starting...\n", appName, serviceName)
		if err := startService(m, serviceName); err != nil {
			log.Fatalf("daemon: %v", err)
		}
		os.Exit(0)
	}

	// Create the service.
	s, err = m.CreateService(
		serviceName,
		cmd,
		mgr.Config{
			DisplayName: serviceName,
			Description: serviceDesc,
			StartType:   mgr.StartAutomatic,
		},
	)
	if err != nil {
		log.Fatalf("daemon: cannot create service '%s': %v", serviceName, err)
	}
	s.Close()

	fmt.Fprintf(os.Stderr, "%s: service '%s' installed, starting...\n", appName, serviceName)

	if err := startService(m, serviceName); err != nil {
		log.Fatalf("daemon: %v", err)
	}
	os.Exit(0)
}

func startService(m *mgr.Mgr, name string) error {
	s, err := m.OpenService(name)
	if err != nil {
		return fmt.Errorf("open service: %w", err)
	}
	defer s.Close()

	err = s.Start()
	if err != nil {
		// Already running is OK.
		if strings.Contains(err.Error(), "already been started") ||
			strings.Contains(err.Error(), "1056") {
			return nil
		}
		return fmt.Errorf("start service: %w", err)
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

// tryRunAsService attempts to run as a Windows service. If successful,
// it blocks until the service stops and returns nil. If not running under
// SCM, it returns an error immediately — the caller falls through to
// console mode. This is more reliable than svc.IsWindowsService() on
// older Windows versions.
func tryRunAsService() error {
	h := &serviceHandler{workerDone: make(chan struct{})}
	err := svc.Run(serviceName, h)
	if err != nil {
		return err
	}
	return h.workerErr
}
