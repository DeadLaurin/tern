//go:build windows

package tools

import (
	"os/exec"
	"syscall"
)

// createNoWindow keeps PowerShell/cmd consoles hidden.
const createNoWindow = 0x08000000

// configureProcessGroup adds CREATE_NO_WINDOW so background jobs do not flash
// a console window.
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}

// killProcessGroup terminates the child process.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// signalProcessGroup terminates the child process. Windows has no portable
// graceful termination for arbitrary commands, so both modes kill.
func signalProcessGroup(cmd *exec.Cmd, force bool) error {
	return killProcessGroup(cmd)
}
