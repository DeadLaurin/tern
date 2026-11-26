package ext

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ScaffoldPlugin creates a ready-to-run plugin directory at
// <parentDir>/<name>/ containing plugin.toml, tool.sh and README.md.
//
// The generated plugin declares a single enabled tool backed by ./tool.sh,
// which reads JSON arguments on stdin and writes a response to stdout. The
// manifest also carries commented examples for [[hooks]] and [[mcp]].
//
// ScaffoldPlugin refuses to overwrite an existing directory and returns the
// created plugin directory.
func ScaffoldPlugin(parentDir, name, description string) (string, error) {
	parentDir = strings.TrimSpace(parentDir)
	if parentDir == "" {
		return "", errors.New("ext: plugin parent directory is required")
	}
	name = strings.TrimSpace(name)
	if err := validatePluginName(name); err != nil {
		return "", err
	}
	description = strings.TrimSpace(description)
	if description == "" {
		description = fmt.Sprintf("A tern plugin named %s.", name)
	}

	dir := filepath.Join(parentDir, name)
	if _, err := os.Lstat(dir); err == nil {
		return "", fmt.Errorf("ext: plugin directory %s already exists", dir)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("ext: stat plugin directory %s: %w", dir, err)
	}
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		return "", fmt.Errorf("ext: create plugins directory: %w", err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", fmt.Errorf("ext: create plugin directory %s: %w", dir, err)
	}

	writeErr := func(err error) (string, error) {
		_ = os.RemoveAll(dir)
		return "", err
	}

	manifest := scaffoldManifest(name, description)
	if err := os.WriteFile(filepath.Join(dir, PluginManifestFile), []byte(manifest), 0o644); err != nil {
		return writeErr(fmt.Errorf("ext: write %s: %w", PluginManifestFile, err))
	}
	toolPath := filepath.Join(dir, "tool.sh")
	if err := os.WriteFile(toolPath, []byte(scaffoldToolScript(name)), 0o755); err != nil {
		return writeErr(fmt.Errorf("ext: write tool.sh: %w", err))
	}
	if err := os.Chmod(toolPath, 0o755); err != nil {
		return writeErr(fmt.Errorf("ext: chmod tool.sh: %w", err))
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(scaffoldReadme(name, description)), 0o644); err != nil {
		return writeErr(fmt.Errorf("ext: write README.md: %w", err))
	}
	return dir, nil
}

func validatePluginName(name string) error {
	if name == "" {
		return errors.New("ext: plugin name is required")
	}
	if err := validateExtensionName(name); err != nil {
		return fmt.Errorf("ext: invalid plugin name %q: names must be a single directory name without path separators", name)
	}
	return nil
}

func scaffoldManifest(name, description string) string {
	return fmt.Sprintf(`name = %q
version = "0.1.0"
description = %q
enabled = true

[[tools]]
name = "hello"
description = "Say hello from the %s plugin."
command = "./tool.sh"
read_only = true
timeout_seconds = 30
parameters = '{"type":"object","properties":{"name":{"type":"string","description":"Who to greet"}},"additionalProperties":false}'

# --- Hooks (optional) ------------------------------------------------------
# Hooks run on agent events and can block or annotate a turn.
#
# [[hooks]]
# event = "user_prompt_submit"
# command = "./hook.sh"
# matcher = ".*"
# timeout = "5s"

# --- MCP servers (optional) ------------------------------------------------
# Plugins can contribute MCP servers over stdio or HTTP.
#
# [[mcp]]
# name = "example"
# transport = "stdio"
# command = "./mcp-server"
# args = []
#
# [[mcp]]
# name = "remote"
# transport = "http"
# url = "https://example.com/mcp"
`, name, description, name)
}

func scaffoldToolScript(name string) string {
	return fmt.Sprintf(`#!/bin/sh
# %s - tern plugin tool.
#
# Contract:
#   stdin  - a single JSON object with the tool arguments
#   stdout - the tool result (plain text or JSON)
#   exit 0 - success; any non-zero exit is reported as a tool error
#
# Environment provided by tern:
#   TERN_EXT_DIR  - this plugin directory
#   TERN_WORK_DIR - the project working directory

set -eu

input=$(cat)

# Tiny, dependency-free extraction of an optional "name" argument.
name=$(printf '%%s' "$input" | sed -n 's/.*"name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')

if [ -n "$name" ]; then
  printf 'Hello, %%s! The %s plugin is working.\n' "$name"
else
  printf 'The %s plugin is working.\n' 
fi

printf 'Plugin directory: %%s\n' "${TERN_EXT_DIR:-unknown}"
`, name, name, name)
}

func scaffoldReadme(name, description string) string {
	return fmt.Sprintf(`# %s

%s

This is a tern plugin: a directory with a `+"`plugin.toml`"+` manifest plus
whatever scripts and assets it needs.

## Layout

- `+"`plugin.toml`"+` - manifest (metadata, tools, hooks, MCP servers)
- `+"`tool.sh`"+` - the example tool, invoked as `+"`./tool.sh`"+` with JSON on stdin
- `+"`README.md`"+` - this file

## Try it

	tern plugin list
	tern plugin guide

The plugin exposes one tool, `+"`ext_%s_hello`"+`. From a tern session ask the
agent to call it, or run the script directly:

	echo '{"name":"Ada"}' | ./tool.sh

## Customize

1. Edit `+"`plugin.toml`"+` to rename the tool or add more `+"`[[tools]]`"+` blocks.
2. Point `+"`command`"+` at any executable in this directory (paths are resolved
   relative to the plugin directory) or at a binary on `+"`PATH`"+`.
3. Uncomment the `+"`[[hooks]]`"+` or `+"`[[mcp]]`"+` examples to extend the agent
   with event hooks or MCP servers.
4. Bump `+"`version`"+` when you change behavior.

## Install elsewhere

	tern plugin install <path-or-git-url>

Installed plugins live in `+"`~/.config/tern/plugins/<name>`"+` (or
`+"`./.tern/plugins/<name>`"+` for project-local plugins).
`, name, description, name)
}

// PluginGuide returns a concise, complete specification of the plugin format
// intended for humans and AI agents.
func PluginGuide() string {
	return `# tern plugins

A plugin is a directory containing a plugin.toml manifest plus optional
scripts/assets. Plugins can contribute agent tools, event hooks and MCP
servers.

## Locations (searched in this order, later wins)

  ~/.config/tern/plugins/<name>       global plugins (primary)
  ./.tern/plugins/<name>              project plugins (primary)
  ~/.config/tern/extensions/<name>    legacy global extensions (fallback)
  ./.tern/extensions/<name>           legacy project extensions (fallback)

Legacy extension.toml manifests are still read when plugin.toml is absent.
Plugins override legacy extensions with the same manifest name; project
overrides global.

## plugin.toml

  name = "my-plugin"           # required, unique, no path separators
  version = "0.1.0"
  description = "What it does"
  author = "you"
  enabled = true               # defaults to true

  # Optional per-tool defaults: timeout_seconds, read_only.

## [[tools]]

  [[tools]]
  name = "hello"               # required; exposed as ext_<plugin>_hello
  description = "Say hello"
  command = "./tool.sh"        # required; relative paths resolve against the plugin dir
  args = []                    # optional argv, appended to command
  read_only = false            # optional; true means no side effects
  timeout_seconds = 30         # optional; default 30
  parameters = '{"type":"object","properties":{},"additionalProperties":false}'

Tool contract: tern runs command with args, writes the tool arguments as a
single JSON object to stdin, and reads the result from stdout (max 64 KiB).
A non-zero exit code, a timeout, or stderr output on failure is reported as a
tool error. The process runs in the project working directory with
TERN_EXT_DIR (plugin directory) and TERN_WORK_DIR (project directory) set.

## [[hooks]]

  [[hooks]]
  event = "user_prompt_submit" # event name (e.g. user_prompt_submit, pre_tool_use)
  command = "./hook.sh"        # relative paths resolve against the plugin dir
  args = []
  matcher = ".*"               # optional regex over the event payload
  timeout = "5s"               # duration ("5s") or seconds

Hooks receive the event as JSON on stdin. A non-zero exit blocks the action
and its stderr is surfaced to the agent.

## [[mcp]]

  [[mcp]]
  name = "local"
  transport = "stdio"          # "stdio" or "http"
  command = "./mcp-server"
  args = []
  env = { KEY = "value" }

  [[mcp]]
  name = "remote"
  transport = "http"
  url = "https://example.com/mcp"
  headers = { Authorization = "Bearer ..." }
  timeout = "30s"

MCP tools are namespaced under the server name.

## Commands

  tern plugin                    # alias for: tern plugin list
  tern plugin list [--json]
  tern plugin new <name> [--local] [--description "..."]
  tern plugin install <path-or-git-url>
  tern plugin remove <name>      # aliases: rm, uninstall
  tern plugin guide              # alias: schema (prints this guide)

` + "`tern plugin new`" + ` scaffolds plugin.toml, tool.sh and README.md into
~/.config/tern/plugins/<name>, or ./.tern/plugins/<name> with --local.
`
}
