// Command tern is a careful coding agent for the terminal.
package main

import (
	"os"

	"golang.org/x/term"
)

// version is replaced at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	opts := defaultRunOptions(os.Args[1:])
	opts.StdinTTY = term.IsTerminal(int(os.Stdin.Fd()))
	opts.StdoutTTY = term.IsTerminal(int(os.Stdout.Fd()))
	os.Exit(runWithOptions(opts))
}
