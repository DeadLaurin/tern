# Changelog

All notable changes to tern are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and tern adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [v0.7.1] - 2026-11-27

### Added
- **`/steps [n]` command**: view or change `max_steps` per turn live in the TUI (alias `/max-steps`).
- **Clickable top-right model switcher in TUI**: clicking the model name under `CONTEXT & STATS` in the right sidebar cycles DeepSeek models immediately (`deepseek-flash` ↔ `deepseek-v4-pro`) or opens `/model` on multi-model providers.
- **Automatic balance refresh**: TUI balance now refreshes automatically after every completed turn and every 2 minutes while idle.

### Changed
- Raised `DefaultMaxSteps` from `50` to `250` in non-YOLO modes and updated the step-limit stop notice to clearly report the limit and how to continue or raise it.
- Replaced horizontal scrolling in the TUI textbar with word-aware soft-wrapping so long prompts and linebreaks behave naturally.

## [v0.7.0] - 2026-11-27

### Added
- **Multi-currency balance checking**: `tern balance [--provider <name>] [--json]`, TUI `/balance` + live sidebar balance, and `tern serve` `/api/balance` + sidebar readout for DeepSeek (showing both CNY and USD with currency symbols/codes, e.g. `¥12.09 CNY · $0.02 USD left`) and OpenRouter (`/credits` with `/auth/key` fallback).
- **Google Gemini (Free Tier) & data-collection notices**: added `gemini` (`gemini-2.5-flash` / `gemini-2.5-pro`, 1M context, free tier via Google AI Studio) alongside OpenRouter `:free` and Groq free tiers, with clear data-collection disclosures in `tern providers`, the TUI `/provider` modal, and `tern serve`.
- **Clickable sub-agent tabs in `tern serve`**: clicking any sub-agent in the `SUB-AGENTS` sidebar opens it in a live tab (`Main` | `<agent>`) displaying its thought process, tool calls, output, and a Cancel button.
- **Full `tern serve` Web UI parity**: interactive `ask_user` prompts (`/api/ask`), live `SUB-AGENTS` and `TASKS` sidebar sections, `/sessions` browser & `/name` rename, `/theme [dark|light]` toggle, interactive `/skills` & `/plugins` modals, and clipboard image paste in the composer.

### Fixed
- **Sub-agent approvals in `tern serve`**: wired `Subagents.Approvals().SetApprover` so sub-agent tool calls prompt the browser instead of failing with `no approval handler configured`.
- **`Allow turn` and `Allow session` in `tern serve`**: `Allow session` and `Allow turn` now actually auto-approve subsequent calls instead of prompting on every tool execution.
- **Chronological tool/response ordering in `tern serve`**: assistant responses and thinking streamed after tool/sub-agent execution now render strictly below the tool/task rows, with human-readable tool durations (`12.4s`, `2m 52s`).

## [v0.6.2] - 2026-11-26

### Added
- **3-step Provider → Connect → Model wizard**: clear visual stepper (`① Provider → ② Connect → ③ Model`) in both the TUI and `tern serve`, automatically transitioning from provider setup into that provider's live model picker, plus a `p` shortcut inside `/model` to switch providers.
- **Full `tern serve` command & modal parity**: added `/api/providers`, `/api/provider`, `/api/models`, `/api/changelog`, `/api/config`, `/api/skills`, `/api/plugins`, and `/api/undo`, along with the interactive Provider → Model modal (with `[FREE]` badges, Free-only toggle, sort, and search), What's New modal, and full slash-command parity (`/provider`, `/model`, `/model free`, `/thinking`, `/tools`, `/copy`, `/changelog`, `/config`, `/skills`, `/plugins`, `/stats`, `/undo`, `/steer`).

### Changed
- **Visible API key input**: pasting or typing an API key in the setup modal now shows the actual key in a clean bordered box (replacing the placeholder cleanly with a character-count confirmation) instead of hiding it behind dots.

### Fixed
- **Windows `Authorization` header error**: sanitized API keys on paste, save, load, and HTTP request construction to strip CR/LF, bracketed-paste ANSI escape sequences, UTF-8 BOMs, and control characters so `net/http` never rejects pasted keys on Windows.

## [v0.6.1] - 2026-11-26

### Added
- **Provider-aware `/model` selector**: fetches live models from the active provider's `/models` endpoint (OpenRouter, OpenAI, Groq, xAI, Mistral, Together, Ollama, LM Studio, or custom endpoints) with instant curated fallbacks, per-provider caching, and `r` to refresh.
- **OpenRouter free-model badge, filter, sort & search**: marks free models with `[FREE]`, defaults OpenRouter to `free-first` sorting, adds **`f`** (and `/model free`) to filter to free models only, **`s`** to cycle sort order (`free-first`, `price`, `context`, `name`), and **`/`** for live substring search.

## [v0.6.0] - 2026-11-26

### Added
- **Multi-provider support**: built-in presets for `deepseek`, `openrouter`, `openai`, `groq`, `xai`, `mistral`, `together`, `ollama`, and `lmstudio`, plus any custom OpenAI-compatible `--base-url` endpoint (`--provider <name>`, `TERN_PROVIDER`, or `provider = "..."` in `config.toml`).
- **Per-provider credentials**: resolves provider-specific env vars (`OPENROUTER_API_KEY`, `OPENAI_API_KEY`, `GROQ_API_KEY`, `XAI_API_KEY`, `MISTRAL_API_KEY`, `TOGETHER_API_KEY`, `DEEPSEEK_API_KEY`, `TERN_API_KEY`) and stores keys per provider under `[providers]` in `~/.config/tern/credentials.toml` while staying backwards-compatible with legacy `api_key`.
- **`tern providers [--json]`**: lists all built-in providers, key status, env vars, default models, and endpoints.
- **First-run TUI onboarding & `/provider` switcher**: launching `tern` with no key configured now opens the TUI immediately with an interactive provider setup modal highlighting DeepSeek as recommended while also offering OpenRouter, other cloud APIs, Ollama, LM Studio, and custom OpenAI-compatible servers.
- **OpenRouter & OpenAI-compatible streaming**: omits DeepSeek-only `thinking` fields on non-DeepSeek APIs, sends OpenRouter attribution headers, streams `delta.reasoning`, and recognizes Claude, Gemini, GPT-4/5, and o3/o4 model capabilities.

### Fixed
- `tern update` no longer dumps release notes into the terminal; it records a pending changelog marker so the `What's New` modal opens reliably once on the next TUI launch.

## [v0.5.1] - 2026-11-26

### Added
- Automatic discovery of Claude (`.claude/skills`) and Agent Skills (`.agents/skills`) directories globally and per-project, including symlinked skill folders.
- `tern skill [list|install|remove]` to pull and manage skills from GitHub browser URLs (`/tree/...`, `/blob/...`), `owner/repo[/subdir]` shorthands, direct raw `SKILL.md` URLs, or local paths.

## [v0.5.0] - 2026-11-26

### Added
- **Plugins** (renamed from extensions, with full backwards compatibility): discover from `~/.config/tern/plugins/` and `./.tern/plugins/` (`plugin.toml`), scaffold starter plugins with `tern plugin new <name>`, and print the AI-friendly spec with `tern plugin guide` (plus `/plugins` in the TUI).
- **Local CSS design system**: customize colors and typography in `~/.config/tern/theme.css` (and optional `./.tern/theme.css`) via standard CSS custom properties applied to both the TUI and `tern serve`, with `tern theme init|check|path|reset` and automatic schema/missing-token warnings across updates.
- **TUI Changelog popup**: scrollable `What's New` modal (`/changelog` or `/whatsnew`) that also opens once automatically after upgrading.
- **TUI drag-to-select & copy**: left-click and drag anywhere in the transcript to highlight and copy text, plus `/copy`, `/mouse` (native terminal selection toggle), and `/tools [compact|expanded|hidden]`.
- **Mobile-optimized `tern serve`**: responsive mobile header + slide-over `CONTEXT & STATS` drawer, `100dvh` + safe-area insets, `16px` composer input, and touch-friendly controls.

### Changed
- `tern serve` now binds to `0.0.0.0:7433` by default so devices on your home network can connect immediately (use `--local-only` for `127.0.0.1`).
- Refined TUI tool calls (`◐`/`✓`/`✗` semantic glyphs, shortened paths, `read` line ranges, `ask_user` question extraction, per-tool click-to-expand), cleaner `▸ Thought` blocks, and sleeker `━`/`─` sidebar gauges.

### Fixed
- Fixed `TestPathLocker` goroutine-ordering flake in `internal/subagent`.

## [v0.4.1] - 2026-11-26

### Changed
- Redesigned the `tern serve` web UI to match the `tern` CLI two-pane terminal aesthetic (welcome emblem, `❯ ┃` transcript, `▸ Thought` blocks, `⚙ tool` lines, `CONTEXT & STATS` block gauges, and clickable bottom status bar).
- Added live `/` command autocomplete with inline ghost text, global terminal-style keyboard focus, `Ctrl+B` / `Ctrl+Y` / `Shift+Tab` shortcuts, and `/api/permission` + `/api/compact` endpoints to `tern serve`.

### Fixed
- Fixed `tern serve` composer focus, welcome hiding on user prompt, and `turn_end` message rendering so typing and submitting work immediately.

## [v0.4.0] - 2026-11-26

### Added
- `tern serve`: lightweight LAN-accessible minimalist web UI with SSE streaming, interactive tool approvals, live context/cache/cost stats, model/effort switching, and optional password login (`--password`, `TERN_SERVE_PASSWORD`, or `--auth`).
- `tern acp`: Agent Client Protocol (JSON-RPC 2.0 over stdio) connector for T3 Code and ACP hosts, with live `usage_update` notifications, `session/stats`, mode/model switching, and bidirectional permission prompts.
- `CHANGELOG.md` and release-notes display in `tern update` / `tern update --check`, plus automatic changelog extraction in the release workflow.

## [v0.3.2] - 2026-11-26

### Added
- Live slash-command autocomplete in the TUI with ghost-text completion, a version indicator, and an update-available badge.
- `tern version` now checks GitHub for newer releases (skippable with `--no-check`) and the TUI update checker runs asynchronously.

## [v0.3.1] - 2026-11-26

### Fixed
- Streaming token deltas now accumulate accurately, cache hits no longer report 0%, and the context gauge no longer clips.
- Provider usage normalizes `cached_tokens`, cache misses, and `total_tokens` from SSE streams.
- Context token accounting retains completion tokens and excludes tool arguments from answer tok/s.

### Changed
- README refreshed with updated source-install instructions and tern.disgusting.work install URLs.
- MIT license added alongside a minimalist site hosting `install.sh` and `install.ps1`.

### Removed
- GitHub Pages workflow, migrated to Cloudflare Pages.

## [v0.3.0] - 2026-11-26

### Added
- `tern doctor` diagnostics command with `--json` output.
- Extension framework (`tern ext list|install|remove`) for tools, hooks, and MCP servers.
- Interactive `/ext` panel and expandable diff stats in the TUI.
- `--local` / `--base-url` offline mode plus `tern undo`, `tern log`, and `tern policy` commands.
- Fast, `.gitignore`-respecting repo indexer injected once on the first turn.
- TUI workspace polish: clickable sidebar, status bar, modals, thinking toggle, right-click copy, and sub-agent thread peek.
- Message queueing, mid-turn steering UX, and a soaring tern emblem.
- `tern uninstall`, and a more robust `tern update` with `--check` and symlink-safe replacement.

### Changed
- `/theme dark|light`, a compaction banner, and an accurate parent context meter.
- Repo indexing and `.git/HEAD` reads now run asynchronously without subprocesses.
- Default step limit raised to 1000 in YOLO mode.

### Fixed
- Parent `ContextTokens` now reports accurately on usage, compaction, and `turn_end` events.
- `duration_ms` is at least 1 on Windows coarse timers when tokens were produced.
- `tern doctor` section headers render in order.
- All `IsParallelSafe` tools, including task sub-agents, run concurrently.

[v0.7.1]: https://github.com/DeadLaurin/tern/compare/v0.7.0...v0.7.1
[v0.7.0]: https://github.com/DeadLaurin/tern/compare/v0.6.2...v0.7.0
[v0.6.2]: https://github.com/DeadLaurin/tern/compare/v0.6.1...v0.6.2
[v0.6.1]: https://github.com/DeadLaurin/tern/compare/v0.6.0...v0.6.1
[v0.6.0]: https://github.com/DeadLaurin/tern/compare/v0.5.1...v0.6.0
[v0.5.1]: https://github.com/DeadLaurin/tern/compare/v0.5.0...v0.5.1
[v0.5.0]: https://github.com/DeadLaurin/tern/compare/v0.4.1...v0.5.0
[v0.4.1]: https://github.com/DeadLaurin/tern/compare/v0.4.0...v0.4.1
[v0.4.0]: https://github.com/DeadLaurin/tern/compare/v0.3.2...v0.4.0
[v0.3.2]: https://github.com/DeadLaurin/tern/compare/v0.3.1...v0.3.2
[v0.3.1]: https://github.com/DeadLaurin/tern/compare/v0.3.0...v0.3.1
[v0.3.0]: https://github.com/DeadLaurin/tern/compare/v0.2.0...v0.3.0
