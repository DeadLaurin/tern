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

Type `tern` in any project folder and you're in. No subcommand ceremony, no heavy runtime, no neon clutter — just a single Go binary, a calm two-pane terminal interface, transparent tools, and files you can read yourself.

---

## Install

**macOS & Linux**

```sh
curl -fsSL https://raw.githubusercontent.com/DeadLaurin/tern/main/scripts/install.sh | sh
```

**Windows (PowerShell)**

```powershell
irm https://raw.githubusercontent.com/DeadLaurin/tern/main/scripts/install.ps1 | iex
```

**From source (Go)**

```sh
go install github.com/tern-agent/tern/cmd/tern@latest
```

To update later, run `tern update`.

---

## Getting Started

```sh
cd your-project
tern
```

On the first run, if `DEEPSEEK_API_KEY` isn't set in your environment, `tern` asks for it once and saves it to your OS config directory with `0600` permissions. On every run after that, it opens straight into your project and automatically picks up any `AGENTS.md` or `TERN.md` files along the way.

Prefer non-interactive or scripted use?

```sh
tern -p "summarize internal/agent/agent.go"
git diff | tern -p "review this diff for subtle bugs"
tern --json -p "find all TODOs"
tern --rpc
tern stats
tern config
```

---

## What It Does

- **Calm two-pane workspace** — Chat on the left, live inspector on the right (context & cache gauges, sub-agent tabs, to-dos, active task). Collapses cleanly on narrow terminals, toggles with `Ctrl+B`, and respects `NO_COLOR`.
- **Built for DeepSeek V4** — Defaults to `deepseek-flash` (1M context, native vision) with `deepseek-v4-pro` one keypress away. Strictly append-only history, deterministic tool schemas, and proper multi-turn `reasoning_content` handling so DeepSeek's automatic prefix cache actually hits.
- **Parallel sub-agents** — Delegates exploration, web research, code review, or isolated edits concurrently (`--max-agents`), with per-path write locks, live status in the right rail, and `Enter` to peek straight into any sub-agent's thread.
- **Queueing & mid-turn steering** — Keep typing while `tern` is working: press `Enter` to queue follow-ups, or `Ctrl+S` to steer the current turn mid-flight.
- **Safe edits & instant undo** — Shadow-copy file snapshots before every edit turn (no git repo needed), so `/undo` and `/rewind` cleanly roll back changes.
- **Plain-file sessions & extensibility** — Append-only JSONL sessions (`--continue`, `--resume`, `/sessions`, `/fork`, `/export`), on-demand skills (`~/.config/tern/skills`, `./.tern/skills`), custom slash commands, lifecycle hooks, and MCP servers (stdio & HTTP).

---

## Keybindings & Commands

| Shortcut / Command | What it does |
| --- | --- |
| `Ctrl+P` / `/model` | Switch model (`deepseek-flash`, `deepseek-v4-pro`) & reasoning level |
| `Shift+Tab` / `/effort` | Cycle reasoning effort (`none`, `low`, `high`, `max`) |
| `Ctrl+T` | Cycle thinking display (`collapsed` → `expanded` → `hidden`) |
| `Ctrl+S` / `/steer` | Steer an active turn mid-flight (or promote a queued message) |
| `Ctrl+Y` / `/yolo` | Toggle allow-all mode on/off (restores previous mode) |
| `Ctrl+B` | Toggle the right inspector sidebar |
| `Tab` → `Enter` | Focus sub-agents in the sidebar and peek into a thread |
| `↑` / `↓` & `Ctrl+R` | Persistent shell-style command history & reverse search |
| `Ctrl+G` | Compose a longer prompt in your `$EDITOR` |
| `Ctrl+C` | Cancel the current turn and sub-agents (twice to quit) |
| `/stats` | Visual usage & activity dashboard (cache gauge, sparklines, cost) |
| `/sessions` | Browse, search, resume, fork, rename, or delete sessions |
| `/undo` · `/rewind` | Restore files to any earlier edit turn |
| `/skills` · `/mcp` | Inspect & load skills or check connected MCP servers |
| `/compact` · `/plan` | Compact context manually or switch to read-only plan mode |
| `/help` | Interactive command & shortcut browser |

---

## Configuration & Files

Configuration layers cleanly in order: `defaults < global < project < env < flags`.

- Global config: `~/.config/tern/config.toml` (or `%APPDATA%\tern` on Windows)
- Project overrides: `./.tern/config.toml`
- Pricing table: `~/.config/tern/pricing.toml` (editable off-peak / peak UTC rates)
- MCP & hooks: `mcp.toml` and `hooks.toml` in global config or `./.tern/`
- Sessions: `~/.local/share/tern/sessions/` (append-only JSONL, including child sub-agent transcripts)

Run `tern config` anytime to inspect effective settings (your API key is always redacted).

---

## Safety

`tern` can run shell commands and edit files. By default it runs in `ask` mode, shows unified diffs before edits, and ships with built-in deny rules blocking destructive commands (`rm -rf` on `/` or `~`, raw device writes, etc.) and sensitive paths (`~/.ssh`, credentials). Web pages, MCP results, and sub-agent reports are treated as untrusted external data and can never escalate permissions.

---

## Development

```sh
go test -race ./...
go vet ./...
go build ./cmd/tern
```

Welcome aboard — pull up a terminal and type `tern`.
