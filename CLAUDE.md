# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

@AGENTS.md

Everything above (layout, commands, rules) applies. The notes below cover what takes several files to see.

## Data flow

```
hooks / statusline ──► go-dock hook|statusline ──POST /api/ingest/{claude|codex}/{hook|statusline}──┐
sessions files, transcripts, rollouts, codex app-server (polled by providers) ──────────────────────┤
                                                                                                    ▼
                                              providers ──model.Observation──► hub (merge, order, TTL)
                                                                                    │ Snapshot + Subscribe
                                       ┌────────────────────────────────────────────┼──────────────────────┐
                                       ▼                                            ▼                      ▼
                       device.BuildFrame → NDJSON over USB          web: /api/state + SSE /api/events   preview tab
                       (firmware/main/link.c → ui.c)                (overview, logs)                    (same frame)

brokers ◄──► mqtt.Pool (one Client per id) ◄── plugins/lights ──Page()──► plugins.Host ──► device.BuildPages ──► `pages` after each snapshot
                                                     ▲ Do(item, act)                                         (+ /api/state.pages, preview)
                                  device `action` / POST /api/plugins/{page}/{item}
```

- Providers never talk to the UI or device; they only emit `model.Observation`s. The hub merges them per session (newer wins), derives `Session.Effective()` (most urgent state of a session and its subagents) and sorts by state priority.
- Observations with `AgentID` set describe a subagent and must not change the parent session's state.
- `device.BuildFrame` is the only place that shapes data for the screen (truncation, `title`/`detail`, waiting-for-subagents text, `today_tokens`). The web "设备预览" tab renders that same frame, so it is the hardware-free check for firmware layout.
- `internal/app` wires everything and implements `web.Backend`; config changes are applied live via `onConfigChange`.
- Plugins (function pages) keep their own state and call `hub.Touch` when their page changes. They reach MQTT through `mqtt.Pool.Client(id)`; config entries name a broker by id (`config.MQTTConfig.BrokerFor` turns an empty id into the first broker), so one broker being down only disables what uses it; the device loop re-reads `plugins.Host.Pages()` on the same debounce as snapshots. `Page()` runs on the serial loop, so it must never wait on the network; `Do` may. The firmware does not render `pages` yet (docs/plans/0002-plugins-mqtt.md, phase 2).

## Things that must stay in sync

- **Screen layout**: the preview in `host/internal/web/static/app.js` (`SCR_*` constants, `scrReset`, `scrTime`, `scrPages`) and `firmware/main/ui.c` (`MAIN_H`, `SUB_H`, `fmt_reset`, `fmt_elapsed`, `layout_pages`) implement the same design, described in `docs/protocol-v1.md` §7.1. Change all three together.
- **Text limits**: `providers.DetailLen` / `TitleLen` bound what the web shows; `device` (`textLen`, `titleLen`) cuts further for the screen. Firmware buffers in `model.h` must fit the device limits, including the page limits (`pageTitleLen`, `itemLabelLen`, …, `maxPages`, `maxItems`).
- **Function page layout**: `app.js` (`scrStrip` dots, `SCR_PAGE_*`, `scrGrid`, `scrCard`) and `docs/protocol-v1.md` §7.1 describe the lights grid; `firmware/main/ui.c` must follow once phase 2 starts.
- **Token numbers** shown to users use `model.Tokens.Fresh()` (no cache reads), both in `BuildFrame` and in `app.js` (`freshTokens`). Costs still include cache reads.

## Running and testing locally

```sh
cd host
go test -race -run TestHookObservation ./internal/providers/claude/   # single test
go build -o bin/go-dock ./cmd/go-dock                                 # local build reports version "dev"
```

The user's own go-dock runs on the default port 47321 with their real `~/.claude` hooks pointing at it. For experiments, run an isolated instance instead:

```sh
mkdir -p /tmp/gd && echo '{"web":{"listen":"127.0.0.1:47399","open_browser":false},"device":{"enabled":false},"update":{"auto_check":false}}' > /tmp/gd/config.json
GO_DOCK_HOME=/tmp/gd CLAUDE_CONFIG_DIR=/tmp/gd/claude CODEX_HOME=/tmp/gd/codex ./bin/go-dock run --no-browser
echo '{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/w/p","tool_name":"Bash","tool_input":{"command":"ls"}}' \
  | ./bin/go-dock hook claude --addr 127.0.0.1:47399
curl -s http://127.0.0.1:47399/api/state   # snapshot + the exact device frame
```

Never install integrations into the real `~/.claude/settings.json` or `~/.codex/hooks.json` while testing; point `CLAUDE_CONFIG_DIR` / `CODEX_HOME` at a temp dir.

MQTT without a real broker: `internal/mqtt/mqtttest` is a small in-process broker (wildcards, retained messages, `Kill()` for reconnect tests). For a manual run, add an entry to `mqtt.brokers` in the temp config pointing at any local broker (e.g. `mosquitto -p 1884`) and simulate a lamp that echoes its command topic to a retained state topic.

Firmware UI checks without hardware: `firmware/sim` compiles `main/ui.c` against LVGL on the desktop and writes PPM screenshots. It needs `idf.py reconfigure` once (downloads LVGL into `managed_components/`) and the cmake from `. ~/esp/esp-idf/export.sh`. Sample scenarios live in `firmware/sim/sim.c`.

## Claude Code data sources (undocumented, parse defensively)

- `~/.claude/sessions/<pid>.json`: live status (`busy|idle|waiting`, `waitingFor`); a file whose PID is no longer alive means the session ended.
- `~/.claude/projects/**/<session>.jsonl`: token usage (deduplicated per `message.id`, max per field), `ai-title` / `custom-title` lines for session titles (a `/rename` title wins), the last assistant text block for `Session.Message` (cleared by the next user prompt); `subagents/agent-<id>.jsonl` + `.meta.json` for subagents without hooks.
- statusLine stdin `rate_limits` is the only source of Claude 5h/7d percentages; go-dock wraps the user's existing statusLine command (`statusline --chain <base64 of original>`).

## Codex data sources (undocumented, parse defensively)

- `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl`, one file per thread (subagents too, linked by `session_id` / `parent_thread_id`). Since Codex 0.147 threads are `"history_mode":"paginated"`: no `user_message` / `agent_message` events, messages come as `item_completed` items (`UserMessage`, `AgentMessage`). Tool calls are `response_item` lines (`function_call` with `arguments` as JSON text, `custom_tool_call` for `apply_patch`), written before the tool runs. Approval requests are never written: only hooks report them.
- `$CODEX_HOME/session_index.jsonl`: thread names (`/rename` or generated after the first prompt), appended; the last non-empty name per `id` wins.
- `$CODEX_HOME/thread-writer-locks/<id>.lock` exists while a thread is open (0.155+); older versions fall back to file mtime.

## Releases

Pushing `vX.Y.Z` publishes a release; a tag with a suffix (`v0.1.2-beta.2`) is published as a prerelease and only offered to users who enabled "接收测试版". The release also attaches the firmware, built with the tag as its version: `go-dock-firmware-v2.bin` (merged image, flash at `0x0` by hand) and `go-dock-firmware-v2-app.bin` (app only).

Firmware updates over USB (`docs/protocol-v1.md` §3.4): `app.ApplyFirmware` → `update.Updater.Firmware` downloads the app image of go-dock's own version → `device.Manager.StartFlash` checks it and hands it to the serve loop, which streams it chunk by chunk (no snapshots meanwhile) → `firmware/main/ota.c` writes the spare OTA slot and restarts → the manager waits for a hello with the new version. Progress is `device.Status.Flash`; the web banner reads it from `state.device`.
