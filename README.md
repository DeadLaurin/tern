```text
▄▄▄▄                                 ▄
 ▀████▄                           ▄███
   ▀██████▄                    ▄█████
     ▀▀██████▄▄▄            ▄▄█████▀
       ▀▀████████▄      ▄▄███████▀
          ▀████████▄ ▄▄██████▀▀
            ███████████████▀
             ▀███████████████▄▄
      ██████████████████████▀▀▀█▄▄
       ██████▀▀▀▀████▀▀▀▀▀
```

# tern

A small, fast, minimalist terminal coding agent for the DeepSeek API.

Type `tern` in any repository and start working. Single static Go binary, ~10 ms startup, calm two-pane TUI, parallel sub-agents, and transparent plain-file state.

Website & installer: **[tern.disgusting.work](https://tern.disgusting.work)**

---

## Install

**macOS & Linux**

```sh
curl -fsSL https://tern.disgusting.work/install.sh | sh
```

**Windows (PowerShell)**

```powershell
irm https://tern.disgusting.work/install.ps1 | iex
```

**From source (Go 1.24+)**

```sh
git clone https://github.com/DeadLaurin/tern.git
cd tern && go install ./cmd/tern
```

Release binaries are SHA-256 verified from [GitHub Releases](https://github.com/DeadLaurin/tern/releases). Update anytime with `tern update` or remove with `tern uninstall`.

---

## Quick Start

```sh
cd your-project
tern
```

On first launch, if `DEEPSEEK_API_KEY` isn't set in your environment, `tern` prompts for it once and saves it to your OS config directory (`0600`). Every launch after that opens straight into your repo and automatically picks up `AGENTS.md` or `TERN.md` instructions.

Want to run against a local model (Ollama or any OpenAI-compatible server)?

```sh
tern --local
```

Prefer non-interactive or scripted usage?

```sh
tern -p "summarize internal/agent/agent.go"
git diff | tern -p "review this diff for subtle bugs"
tern --json -p "find all TODOs"
tern --rpc
```

---

## Highlights

- **Minimal look, hidden depth** — Calm two-pane TUI (chat on the left, live inspector on the right with context & cache gauges, clickable sub-agent tabs, to-dos, and right-click copy). Collapses cleanly on narrow terminals, toggles with `Ctrl+B`, and respects `NO_COLOR`.
- **Prefix-cache first for DeepSeek V4** — Defaults to `deepseek-flash` (1M context, native vision) with `deepseek-v4-pro` one keypress away. Strictly append-only history, deterministic tool schemas, and multi-turn `reasoning_content` passback keep DeepSeek's automatic prefix cache warm.
- **Parallel sub-agents** — Delegates exploration, research, code review, or edits concurrently (`--max-agents`), with per-path write locks, live sidebar status, and one-click thread peeking.
- **Queueing & mid-turn steering** — Keep typing while `tern` works: press `Enter` to queue follow-ups, or `Ctrl+S` to steer the active turn mid-flight without losing context.
- **Instant undo without git** — Shadow-copy file checkpoints before every edit turn so `/undo`, `/rewind`, or `tern undo` cleanly roll back changes in any directory.
- **Extensible & transparent** — Plain JSONL sessions (`--continue`, `--resume`, `/sessions`, `/fork`, `/export`), on-demand skills, lifecycle hooks, MCP servers (stdio & HTTP), and folder-based extensions (`tern ext`, `/ext`).

---

## CLI & Subcommands

| Command | Purpose |
| --- | --- |
| `tern` | Launch the interactive TUI in the current directory |
| `tern -p "..."` | Run a single non-interactive prompt (supports piped stdin) |
| `tern --continue` / `--resume <id>` | Continue the latest session or resume a specific one |
| `tern --json` / `--rpc` | Stream structured JSONL events or run as a JSON-RPC server |
| `tern --local` | Connect to a local Ollama / OpenAI-compatible endpoint |
| `tern doctor` | Check environment, API key status, toolchain (`rg`, `git`), and workspace |
| `tern stats` | Show token usage, cache hit rate, and estimated cost |
| `tern config` | Inspect effective layered settings (API key always redacted) |
| `tern undo` / `tern log` | Restore edit checkpoints or inspect recent session activity |
| `tern ext` | List, install, enable, disable, or remove folder extensions |
| `tern policy` | Inspect active permission and deny rules |
| `tern update` / `tern uninstall` | Self-update from GitHub Releases or cleanly uninstall |

---

## Keybindings & Slash Commands

| Shortcut / Command | What it does |
| --- | --- |
| `Ctrl+P` / `/model` | Switch model (`deepseek-flash`, `deepseek-v4-pro`) & reasoning level |
| `Shift+Tab` / `/effort` | Cycle reasoning effort (`none`, `low`, `high`, `max`) |
| `Ctrl+T` | Cycle thinking display (`collapsed` → `expanded` → `hidden`) |
| `Ctrl+S` / `/steer` | Steer an active turn mid-flight (or promote a queued message) |
| `Ctrl+Y` / `/yolo` | Toggle allow-all mode on/off (restores previous mode) |
| `Ctrl+B` | Toggle the right inspector sidebar |
| `Tab` → `Enter` | Focus sub-agents in the sidebar and peek into a thread |
| `↑` / `↓` & `Ctrl+R` | Persistent shell-style input history & reverse search |
| `Ctrl+G` | Compose a longer prompt in `$EDITOR` |
| `Ctrl+C` | Cancel the active turn and sub-agents (twice to quit) |
| `/stats` | Usage dashboard with cache gauge, sparklines, and cost |
| `/sessions` | Browse, search, resume, fork, rename, or delete sessions |
| `/undo` · `/rewind` | Restore files to any earlier edit turn |
| `/skills` · `/ext` · `/mcp` | Inspect loaded skills, extensions, and connected MCP servers |
| `/compact` · `/plan` | Compact context manually or switch to read-only plan mode |
| `/help` | Interactive command & shortcut browser |

---

## Configuration & Files

Configuration merges in order: `defaults < global < project < env < flags`.

- **Global config**: `~/.config/tern/config.toml` (or `%APPDATA%\tern` on Windows)
- **Project overrides**: `./.tern/config.toml`
- **Pricing table**: `~/.config/tern/pricing.toml` (editable off-peak / peak UTC rates)
- **Extensions, skills, MCP, hooks**: `~/.config/tern/` or `./.tern/` (`extensions/`, `skills/`, `mcp.toml`, `hooks.toml`)
- **Sessions**: `~/.local/share/tern/sessions/` (append-only JSONL, including child sub-agent transcripts)

---

## Safety

By default `tern` runs in `ask` mode, shows unified diffs before applying edits, and enforces built-in deny rules against destructive shell commands (`rm -rf` on `/` or `~`, raw device writes, etc.) and sensitive paths (`~/.ssh`, credentials). Web pages, MCP outputs, and sub-agent reports are treated as untrusted data and can never escalate permissions.

---

## Development

```sh
go test -race ./...
go vet ./...
go build ./cmd/tern
```

## License

[MIT](LICENSE)
