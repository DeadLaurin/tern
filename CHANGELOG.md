# Changelog

All notable changes to tern are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and tern adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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

[v0.5.1]: https://github.com/DeadLaurin/tern/compare/v0.5.0...v0.5.1
[v0.5.0]: https://github.com/DeadLaurin/tern/compare/v0.4.1...v0.5.0
[v0.4.1]: https://github.com/DeadLaurin/tern/compare/v0.4.0...v0.4.1
[v0.4.0]: https://github.com/DeadLaurin/tern/compare/v0.3.2...v0.4.0
[v0.3.2]: https://github.com/DeadLaurin/tern/compare/v0.3.1...v0.3.2
[v0.3.1]: https://github.com/DeadLaurin/tern/compare/v0.3.0...v0.3.1
[v0.3.0]: https://github.com/DeadLaurin/tern/compare/v0.2.0...v0.3.0
