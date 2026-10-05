//go:build !windows

package cli

import (
	"os"
	"os/exec"
	"syscall"
)

// detach starts cmd in its own session, so it outlives the terminal.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// terminate asks the gateway to shut down cleanly.
func terminate(p *os.Process) error { return p.Signal(syscall.SIGTERM) }
