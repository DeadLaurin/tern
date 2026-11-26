package main

// This file keeps the legacy `tern ext ...` command working. All plugin and
// extension subcommands are implemented in plugin.go; `ext` is a
// backwards-compatible alias for `plugin`.

// runExtCommand dispatches `tern ext ...` and delegates to the plugin command.
func runExtCommand(args []string, o runOptions) int {
	return runPluginCommand(args, o)
}
