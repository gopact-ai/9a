//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

// Package processgroup starts child processes in their own process group and
// kills the whole group, so that a subprocess and any children it spawns are
// terminated together.
package processgroup

import (
	"os/exec"
	"syscall"
)

func Configure(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func Kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}
