//go:build windows

package main

import (
	"fmt"
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
func reexecAsDaemon() {
	exePath, err := os.Executable()
	if err != nil {
		log.Fatalf("daemon: os.Executable() failed: %v", err)
	}
	fmt.Fprintf(os.Stderr, "%s: daemon: executable=%s\n", appName, exePath)
	fmt.Fprintf(os.Stderr, "%s: daemon: args=%v\n", appName, os.Args[1:])

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

	// Build the full command line for the service binary path.
	cmd := fmt.Sprintf(`"%s" %s`, exePath, strings.Join(svcArgs, " "))
	fmt.Fprintf(os.Stderr, "%s: daemon: service binary path: %s\n", appName, cmd)

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

	// Create the service.
	fmt.Fprintf(os.Stderr, "%s: daemon: calling CreateService('%s', ...)\n", appName, serviceName)
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
		log.Fatalf("daemon: CreateService('%s') failed: %s", serviceName, winErr(err))
	}
	fmt.Fprintf(os.Stderr, "%s: daemon: CreateService('%s') OK\n", appName, serviceName)
	s.Close()

	fmt.Fprintf(os.Stderr, "%s: daemon: calling startService('%s')...\n", appName, serviceName)
	if err := startService(m, serviceName); err != nil {
		// On some Windows versions (notably Win7), s.Start() returns
		// ACCESS_DENIED even as Administrator. The service IS installed
		// correctly — just start it manually.
		fmt.Fprintf(os.Stderr, "%s: daemon: s.Start() returned: %v\n", appName, err)
		fmt.Fprintf(os.Stderr, "%s: Service '%s' is installed but could not be started automatically.\n", appName, serviceName)
		fmt.Fprintf(os.Stderr, "%s: Start it manually:\n", appName)
		fmt.Fprintf(os.Stderr, "%s:   sc start %s\n", appName, serviceName)
		fmt.Fprintf(os.Stderr, "%s: Or reboot — it's set to StartAutomatic.\n", appName)
		os.Exit(0)
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
