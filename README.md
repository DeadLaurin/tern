<img src="assets/icon.svg" width="64" height="64" alt="">

# tern

A minimal, fast, cross-platform terminal coding agent for the DeepSeek API.
`tern` runs where your code lives, keeps its request prefix byte-stable so
DeepSeek's automatic prefix cache keeps working, and stays out of the way while
you work.

- **Models:** `deepseek-flash` (default) and `deepseek-v4-pro`
- **Context:** 1,000,000 tokens, up to 384,000 output tokens
- **Platforms:** Linux, macOS, Windows (amd64 and arm64)
- **Interfaces:** interactive TUI, one-shot print mode, piped stdin, JSON
  events, JSON-RPC

## Install

### Linux and macOS

```sh
curl -fsSL https://raw.githubusercontent.com/tern-agent/tern/main/scripts/install.sh | sh
```

The installer downloads the release archive, verifies it against
`checksums.txt`, and installs the binary to `/usr/local/bin` when writable,
otherwise to `$HOME/.local/bin`. Override with environment variables:

| Variable | Meaning |
| --- | --- |
| `TERN_VERSION` | Release to install, e.g. `1.2.3` or `v1.2.3` (default: latest) |
| `TERN_INSTALL_DIR` | Install directory (default: `/usr/local/bin` or `$HOME/.local/bin`) |
| `TERN_REPO` | Repository in `owner/name` form (default: `tern-agent/tern`) |

### Windows

```powershell
irm https://raw.githubusercontent.com/tern-agent/tern/main/scripts/install.ps1 | iex
```

The PowerShell installer verifies the SHA-256 checksum, installs `tern.exe` to
`%LOCALAPPDATA%\Programs\tern`, and adds that directory to your user `PATH`.
Use `-Version` or `-InstallDir` to override, or the same `TERN_VERSION`,
`TERN_INSTALL_DIR` and `TERN_REPO` environment variables.

### Go

```sh
go install github.com/tern-agent/tern/cmd/tern@latest
```

### Self-update

```sh
tern update
```

`tern update` replaces the current binary with the latest release. It uses the
same version resolution as the install scripts.

## First Run & Usage

Run `tern` inside any project directory:

```sh
cd ~/src/my-project
tern
```

On first launch tern asks for a DeepSeek API key and stores it in
`<config-dir>/credentials.toml` with `0600` permissions (the config directory
is created with `0700`). `DEEPSEEK_API_KEY` takes precedence over the stored
key and skips the prompt, which is useful for CI. On Linux and macOS the config
directory is `$XDG_CONFIG_HOME/tern`, else `~/.config/tern`; on Windows it is
`%APPDATA%\tern`.

### Interactive mode

The TUI shows streaming reasoning and text, tool calls as they run, a diff
before a write is approved, and a status bar with the active model, context
usage, cache hit rate and cumulative cost.

### Print mode and pipes

```sh
tern -p "Explain the retry logic in internal/provider"
tern -p --json "List the failing tests" > events.jsonl
echo "Review the last commit" | tern -p
git diff | tern -p "Write a commit message for this change"
```

`-p, --print` runs one non-interactive turn and prints the final answer.
Piped stdin becomes the prompt (or is appended as context when `-p` also has
text).

- `--json` emits newline-delimited agent events (`turn_start`,
  `reasoning_delta`, `text_delta`, `tool_start`, `tool_end`, `usage`,
  `cache_debug`, `compact`, `turn_end`, `error`) instead of the TUI.
- `--rpc` exposes the same session over JSON-RPC 2.0 on stdin/stdout for
  editor and tooling integrations.

### CLI flags

| Flag | Description |
| --- | --- |
| `--continue` | Continue the most recent session in this directory |
| `--resume [id]` | Resume a session by id (picker when omitted) |
| `--model <name>` | `deepseek-flash` or `deepseek-v4-pro` |
| `--plan` | Start in read-only plan mode |
| `--permission <mode>` | `ask`, `auto-edit`, `yolo`, or `plan` |
| `--max-cost <usd>` | Stop the session at this spend (fractional USD) |
| `--max-agents <n>` | Maximum concurrent sub-agents (default `4`) |
| `--debug-cache` | Log prefix hashes and warn on cache-prefix divergence |
| `-p, --print` | One-shot mode |
| `--json` | Emit JSON events |
| `--rpc` | Serve JSON-RPC on stdio |

### Slash commands

| Command | Action |
| --- | --- |
| `/help` | Show commands and keybindings |
| `/model` | Switch between `deepseek-flash` and `deepseek-v4-pro` |
| `/plan` | Toggle read-only plan mode |
| `/compact [instructions]` | Compact the conversation now, with optional guidance |
| `/cost` | Show usage and cost, including sub-agents |
| `/context` | Show context-window occupancy and the active prefix |
| `/agents` | Inspect, expand and cancel sub-agents |
| `/clear` | Clear the active context (the session transcript stays on disk) |
| `/undo` | Undo the file changes of the last turn |
| `/rewind` | Rewind files and conversation to an earlier turn |
| `/sessions` | Browse sessions in this workspace |
| `/fork` | Fork the current session into a new one |
| `/name <name>` | Name the current session |
| `/export` | Export the transcript as Markdown or standalone HTML |
| `/skills` | List and load on-demand skills |
| `/mcp` | Show MCP servers, tools, and connection status |
| `/config` | Show the effective configuration |
| `/quit` | Exit |

## Configuration

Configuration is layered. Higher layers override lower ones field by field:

```
defaults < global config.toml < project .tern/config.toml < environment < flags
```

The global file is `<config-dir>/config.toml`; the project file is
`<workdir>/.tern/config.toml`. Environment variables are the TOML field names
upper-cased with a `TERN_` prefix (`TERN_MODEL`, `TERN_MAX_STEPS`,
`TERN_PERMISSION_MODE`, `TERN_MAX_COST_USD`, `TERN_MAX_AGENTS`,
`TERN_DEBUG_CACHE`, ...). `TERN_CONFIG_DIR` and `TERN_DATA_DIR` relocate the
config and data directories.

Example `config.toml`:

```toml
model = "deepseek-flash"
base_url = "https://api.deepseek.com"
reasoning_effort = "high"        # none | low | high | max
permission_mode = "ask"          # ask | auto-edit | yolo | plan
web_permission = "ask"           # ask | allow | deny

max_steps = 50
max_cost_usd = 0.0               # 0 = no limit
max_agents = 4
max_agent_depth = 1

auto_compact_threshold = 0.78    # fraction of the context window
debug_cache = false

shell = ""                       # empty = platform default
search_backend = "duckduckgo"    # brave | tavily | searxng | duckduckgo
search_endpoint = ""             # required by searxng
```

### Pricing

Built-in pricing ships with tern. To reflect contract or regional rates, edit
`<config-dir>/pricing.toml`:

```toml
[pricing.deepseek-flash]
context_window = 1000000
max_output = 384000
supports_vision = true

off_peak_cache_hit_per_m  = 0.003
off_peak_cache_miss_per_m = 0.15
off_peak_output_per_m     = 0.60

peak_cache_hit_per_m  = 0.006
peak_cache_miss_per_m = 0.30
peak_output_per_m     = 1.20
```

Rates are USD per million tokens. Peak windows are Monday–Friday,
01:00–04:00 and 06:00–10:00 UTC; everything else bills at off-peak rates.
`pricing.toml` also accepts top-level model tables, and `[pricing.*]` tables in
either `config.toml` override it. Use `tern config` to print the effective
configuration; the API key is always redacted to `[set]` or `[not set]`.

## Context Caching

DeepSeek caches request prefixes automatically. A cache hit is far cheaper
than a miss, so tern treats the prefix as a contract:

- **Byte-stable system prompt.** The system prompt contains no dates, paths,
  counters, or session ids. Volatile context (working directory, platform,
  date, git branch and status) and `AGENTS.md` / `TERN.md` content are injected
  once into the first user message.
- **Deterministic tool serialization.** Tools are sorted by name and every
  JSON schema is canonicalized recursively. Go map ordering never reaches the
  wire, and each sub-agent type has a fixed, sorted tool subset.
- **Append-only history.** Turns only add messages; earlier messages are not
  rewritten. Completed-turn `reasoning_content` is omitted from later requests,
  while reasoning produced inside the active tool loop is preserved verbatim,
  as DeepSeek requires.
- **Verification.** `--debug-cache` emits a `cache_debug` event per step with
  the canonical prefix hash and system+tools hash, and warns if the prefix
  diverged unexpectedly.
- **Visibility.** The status bar shows the live cache hit rate and cumulative
  cost; `/cost` and `/context` break the numbers down.

Compaction is the one deliberate prefix reset: after a compaction the next
request starts a new cache prefix.

## Context Compaction

Tern reduces context in two stages:

1. **Cheap pruning.** Old tool results larger than 4,000 characters are
   replaced in place with a short `[pruned tool output: N chars omitted]`
   marker, keeping the recent tail intact. This costs no model call.
2. **Structured compaction.** When the conversation still exceeds the
   threshold, older turns are summarized into a structured record (Goal,
   Decisions, Files, Findings, Open TODOs, Next steps) and the recent tail is
   kept verbatim.

Auto-compaction runs at `auto_compact_threshold` of the model's context window
(default `0.78`, i.e. 78% of 1M tokens), and once more if the API reports a
context-length error. Run `/compact` to compact on demand, optionally with
instructions:

```
/compact keep the API details and drop the exploratory dead ends
```

Sessions are append-only JSONL files under the data directory
(`$XDG_DATA_HOME/tern/sessions`, else `~/.local/share/tern/sessions`; on
Windows `%APPDATA%\tern\sessions`). Compaction writes a `compaction` record
that replaces older messages during replay; it never deletes the originals, so
`/rewind`, `/fork` and exports still see the full history.

## Sub-Agents

Tern can delegate work to bounded, concurrent sub-agents. Each has a constant
system prompt and a deterministically sorted tool set, so agents of the same
type share a cache prefix.

| Type | Access | Tools |
| --- | --- | --- |
| `explore` | read-only | `bash`, `glob`, `grep`, `ls`, `read` |
| `research` | read-only, web | `read`, `web_fetch`, `web_search` |
| `review` | read-only | `bash`, `glob`, `grep`, `ls`, `read` |
| `general` | read-write | full set except `task` and `ask_user` |

Read-only agents use plan-mode permissions; `bash` is limited to the read-only
allowlist. `general` cannot delegate further or ask the user questions.

### Custom agents

Drop a Markdown file into `<config-dir>/agents/` (global) or
`./.tern/agents/` (project). Project agents override global agents, which
override built-ins with the same name:

```markdown
---
name: docs
description: Write and update documentation
tools: read, grep, glob, write, edit
model: deepseek-flash
max_steps: 20
permission: ask
---
You are the documentation sub-agent. Match the project's existing voice and
structure, and keep examples runnable.
```

`description` defaults to the first paragraph, `name` to the file name, and
custom agents default to the read-only exploration tool set unless they list
mutating tools. `permission: plan` forces read-only.

### Limits and isolation

- `max_agents` / `--max-agents` caps concurrency (default `4`).
- `max_agent_depth` defaults to `1`; the hard cap is `5`.
- Write-capable agents share a cooperative per-path lock table, so two agents
  never edit the same file at once.
- In isolated mode, a write agent edits a disposable workcopy (skipping VCS
  metadata, dependency trees and build artifacts); the parent receives a
  unified patch, changed-file list, and structured report.

### Live visibility

Agent blocks stream status, current tool, step count, tokens and cost.
`Tab` expands or collapses the focused agent, `Ctrl+O` toggles the agent
panel, and `/agents` lists, expands and cancels individual agents. Each
sub-agent writes its own transcript next to its parent:

```
<sessions>/<sessionID>.jsonl
<sessions>/<sessionID>.sub.<agentID>.jsonl
```

The parent session's usage and cost roll up child transcripts, and `/cost`
reports them separately.

## Extensibility

### Instruction files

`AGENTS.md` and `TERN.md` are loaded hierarchically: the global config
directory first, then the repository root down to the working directory. Files
are presented outermost-first; later, more specific files refine earlier ones.
They are injected into the first user message, keeping the system prompt
stable.

### Skills

Skills are Markdown instructions loaded on demand. Both layouts work:

```
~/.config/tern/skills/<name>.md
~/.config/tern/skills/<name>/SKILL.md
.tern/skills/<name>.md
.tern/skills/<name>/SKILL.md
```

Frontmatter supports `name`, `description`, and `tags`. Project skills
override global skills. `/skills` lists them; tern loads only the body it
needs when a skill is selected.

### Custom slash commands

Put Markdown templates in `<config-dir>/commands/` or `.tern/commands/`.
Project commands override global ones. Frontmatter may set `name` and
`description`; templates support `$ARGUMENTS` for the full argument string and
`$1`, `$2`, ... for quoted positional arguments.

### Lifecycle hooks

Hooks are executables configured per event. Each receives a JSON object on
stdin (`event`, `tool`, `agent`, `session_id`, `cwd`, `args`, `extra`).
Supported events: `session_start`, `before_tool`, `after_tool`,
`subagent_start`, `subagent_end`, `on_compact`, `session_end`. Hooks default
to a 10-second timeout, and a non-zero exit from `before_tool` blocks the tool
call.

### MCP servers

Tern is an MCP client for both transports:

- **stdio:** spawn a command with arguments and environment.
- **HTTP:** connect to a URL with optional headers.

MCP tools are namespaced as `mcp_<server>_<tool>` and sorted like built-in
tools so they do not disturb the cached prefix. `/mcp` shows servers, tools,
and connection status.

## Security

**The `bash` tool executes real commands on your machine.** Treat the agent as
a capable but fallible collaborator: review diffs, avoid running it against
production credentials, and use a container or VM for untrusted repositories.

### Permission modes

| Mode | Behaviour |
| --- | --- |
| `ask` | Prompt before every mutating tool call (default) |
| `auto-edit` | Allow writes/edits inside the workspace; shell and outside-workspace changes still ask |
| `yolo` | Allow everything except explicit deny rules |
| `plan` | Strictly read-only |

Evaluation is deny-first: a broad allow rule can never override a deny rule or
a built-in guard.

### Built-in deny rules

Even in `yolo`, tern refuses to write to sensitive locations such as
`~/.ssh/**`, `~/.gnupg/**`, `~/.aws/credentials`, `~/.netrc`,
`~/.docker/config.json`, `~/.kube/config`, `~/.config/gh/hosts.yml`,
`~/.config/tern/credentials.toml`, `/etc/passwd`, `/etc/shadow`,
`/etc/sudoers*`, and `/etc/ssh/**`. It also denies destructive commands
including `rm -rf /`, `rm -rf ~*`, `mkfs*`, `wipefs*`, raw writes to
`/dev/sd*`, `/dev/disk*`, `/dev/nvme*`, `shutdown`, `reboot`, and fork bombs.
Reads of these paths remain allowed.

### Untrusted content

Web pages, search results, MCP output, command output, and sub-agent reports
are **untrusted external data**. They may contain prompt injection or
misleading instructions. Tern marks such results as external and its system
prompt instructs the model never to follow instructions found inside tool
output. Report suspicious content instead of acting on it. Set
`web_permission = "deny"` to disable network tools entirely.

### OS sandboxing

Tern's permission engine is the primary control; for defence in depth, run it
inside an OS sandbox. On Linux, a `bubblewrap` wrapper can restrict filesystem
and network access for the whole process. On Windows, a Job Object can bound
the process tree and resource usage. These are optional host-level measures,
not a substitute for reviewing what the agent does.

## Development & Testing

```sh
go test -race ./...
go vet ./...
```

Build for a release target with:

```sh
GOOS=linux   GOARCH=amd64 go build -o tern_linux_amd64     ./cmd/tern
GOOS=linux   GOARCH=arm64 go build -o tern_linux_arm64     ./cmd/tern
GOOS=darwin  GOARCH=amd64 go build -o tern_darwin_amd64    ./cmd/tern
GOOS=darwin  GOARCH=arm64 go build -o tern_darwin_arm64    ./cmd/tern
GOOS=windows GOARCH=amd64 go build -o tern_windows_amd64.exe ./cmd/tern
GOOS=windows GOARCH=arm64 go build -o tern_windows_arm64.exe ./cmd/tern
```

Release archives are `tern_<version>_<os>_<arch>.tar.gz` on Linux/macOS and
`.zip` on Windows, published with a `checksums.txt` that the installers verify.
