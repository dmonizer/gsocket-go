package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"
)

// runWatchdog runs the actual worker as a child process, monitoring it and
// restarting on crash with backoff. This is called when -W is set and
// _GSOCKET_WORKER is NOT set (i.e. we are the watchdog parent).
//
// Matches C's gs_watchdog() + GS_daemonize() restart loop:
//   - If child ran >60s → immediate restart
//   - If child exits with EX_BAD_AUTH (201) twice consecutively → exit
//     (another daemon with the same secret is already listening)
//   - Otherwise wait 60s before restart
func runWatchdog() {
	args := os.Args
	env := os.Environ()
	env = append(env, envWorker+"=1")

	// Carry forward daemon-child marker so the worker knows to detach.
	if os.Getenv(envDaemonChild) != "" {
		env = append(env, envDaemonChild+"=1")
	}

	var (
		nBadAuth     int
		maxBadAuth   = 2
		lingerSec    = 13 // GSRN token linger (~10s) + margin, matches C's n=GSRN_TOKEN_LINGER_SEC+3
		restartDelay = 60 * time.Second
	)

	for {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Env = env
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		startTime := time.Now()
		err := cmd.Start()
		if err != nil {
			log.Printf("watchdog: failed to start worker: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		// Wait for child to exit.
		err = cmd.Wait()
		elapsed := time.Since(startTime)

		// Determine exit code.
		exitCode := getExitCode(err)

		// --- Restart logic (matching C's GS_daemonize loop) ---
		delay := restartDelay // default: 60s

		if exitCode == exitBadAuth {
			nBadAuth++
			// Two consecutive BAD_AUTH exits → another daemon is already
			// listening with the same secret. Stop the watchdog.
			if nBadAuth >= maxBadAuth {
				log.Printf("watchdog: two consecutive BAD_AUTH exits — another daemon is already listening. Exiting.")
				os.Exit(0)
			}
			// First BAD_AUTH: wait just long enough for GSRN to drop the
			// stale auth token, then retry.
			delay = time.Duration(lingerSec) * time.Second
		} else {
			nBadAuth = 0
			// If the child ran for more than 60 seconds, restart immediately.
			if elapsed > 60*time.Second {
				delay = 1 * time.Second
			}
		}

		fmt.Fprintf(os.Stderr, "%s ***DIED*** (exit=%d, elapsed=%v). Restarting in %v.\n",
			time.Now().Format("2006-01-02 15:04:05"), exitCode, elapsed.Round(time.Second), delay.Round(time.Second))

		time.Sleep(delay)
	}
}
