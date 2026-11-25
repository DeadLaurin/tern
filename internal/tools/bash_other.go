//go:build !unix && !windows

package tools

import "os/exec"

// configureProcessGroup is a no-op on platforms without Unix process groups.
func configureProcessGroup(cmd *exec.Cmd) {}

// killProcessGroup terminates the child process.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// signalProcessGroup terminates the child process; graceful signals are not
// portable here, so both modes kill.
func signalProcessGroup(cmd *exec.Cmd, force bool) error {
	return killProcessGroup(cmd)
}
