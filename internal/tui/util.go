package tui

import (
	"encoding/base64"
	"os/exec"
)

var base64Encoding = base64.StdEncoding

func execCommand(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}
