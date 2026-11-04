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
                       device.BuildFrame → NDJSON over TCP (WiFi)   web: /api/state + SSE /api/events   preview tab
                       (firmware/main/link.c → ui.c)                (overview, logs)                    (same frame)

sysmon ──► plugins/perf ──Readings()──► plugins.Host.Line() ──► device.BuildLine ──► `line` after each snapshot (+ /api/state.line, preview)
```

- Providers never talk to the UI or device; they only emit `model.Observation`s. The hub merges them per session (newer wins), derives `Session.Effective()` (most urgent state of a session and its subagents) and sorts by state priority.
- Observations with `AgentID` set describe a subagent and must not change the parent session's state.
- `device.BuildFrame` is the only place that shapes data for the screen (truncation, `title`/`detail`, waiting-for-subagents text, `today_tokens`). The web "设备预览" tab renders that same frame, so it is the hardware-free check for firmware layout.
- `internal/app` wires everything and implements `web.Backend`; config changes are applied live via `onConfigChange`.
- Plugins (the line of readings above the usage) keep their own state and call `hub.Touch` when their readings change; the device loop re-reads `plugins.Host.Line()` on the same debounce as snapshots and resends `line` only when it changed. `Readings()` runs on the device loop, so it must never block. Readings take no taps; there are no function pages. The firmware knows nothing of the plugins: it draws whatever `line` holds, three at a time, turning through groups of three by its own clock.

## Things that must stay in sync

- **Screen layout**: the preview in `host/internal/web/static/app.js` (`SCR_*` constants, `scrReset`, `scrTime`, `scrPages`) and `firmware/main/ui.c` (`MAIN_H`, `SUB_H`, `fmt_reset`, `fmt_elapsed`, `layout_pages`) implement the same design, described in `docs/protocol-v1.md` §7.1. Change all three together.
- **Text limits**: `providers.DetailLen` / `TitleLen` bound what the web shows; `device` (`titleLen`, `fitLine`, `agentTextLen`) cuts further for the screen. Firmware buffers in `model.h` must fit the device limits, including the line limits (`maxReadings`, `readingLabelLen`, `readingTextLen`, `readingUnitLen`).
- **Beeps**: the alert melodies live only in `device.Sounds` (the firmware plays whatever notes arrive); their limits (`maxNotes`, `minHz`…`maxMs`) match `BEEP_MAX_NOTES` in `firmware/main/beep.h` and the clamping in `link.c` `handle_beep`; the longest beep must fit `MaxLine` (`TestBeepFitsLine`), and the custom-notes inputs in `index.html` (`maxlength`, the help text) must take that many notes. The web settings list and browser preview read `GET /api/device/sounds`; a custom sound is the user's own notes (`device.beep_custom`, parsed by `device.ParseNotes`, checked when the settings are saved). The volume (`device.volume`) rides in each `beep` as `vol`; the firmware turns it into a PWM duty (`beep.c` `volume_duty`) and the browser preview (`app.js` `playNotes`) follows the same 0.5 dB-per-percent curve.
- **Reading line layout**: `app.js` (`scrLine`, `scrReading`, the turning in `SCR_LINE_*` / `scrLineOffset`, `SCR_ICONS` for reading icons, the `.lined` sizes in `app.css`), `firmware/main/ui.c` (`READ_*`, `fill_seg`, `tick_line`, `take_line`, `layout_usage` for the tighter usage card, the `ICON_*` stroke tables and `ICON_COLOR` drawn by `draw_strokes`) and `docs/protocol-v1.md` §7.1 («读数条») implement the same line; change all three together. The turning follows the clock (6 s per group, 0.5 s slide, cubic ease in and out), never the redraws, so a new `line` only updates texts and bars; the firmware uses two group objects (the one shown and the next sliding in), the preview a track with the first group repeated. `firmware/sim` renders three readings, six turning (and mid-slide), five, the line gone, and the update question with taps on its buttons.
- **Screen brightness**: go-dock keeps the levels (`config.DeviceConfig`, checked in `validate`) and sends them as `settings`; the firmware decides when to dim (`ui.c` `brightness()`) and keeps the last values in NVS (`settings.c`). The ranges in `validate`, the clamping in `link.c` `handle_settings` and `settings_init`, and the options of the selects in `web/static/index.html` must agree.
- **Token numbers** shown to users use `model.Tokens.Fresh()` (no cache reads), both in `BuildFrame` and in `app.js` (`freshTokens`). Costs still include cache reads.
- **WiFi link** (`docs/protocol-v1.md` §1.1–1.2, plan `docs/plans/0004-wifi.md`): everything the screen shows goes over TCP 47322 (`device.Manager.runNet` dials `device.addr` with the pairing code in `hello`); the USB cable (`runUSB`, `device.port`) only carries `wifi`, `wifi_scan`/`wifi_list`, `net` and firmware updates, and the firmware ignores data on it (`link.c` `handle_usb`). Firmware updates also go over WiFi, see Releases. Up to 4 go-docks share the screen, a page each (`link.c` keeps each one's snapshot and line by link slot; same hello `id` on a new link takes over). Whose page shows is `hosts.c` `host_pick` (news elsewhere after 30 s, after 5 s when it waits for the user; without news the pages with tasks take turns every minute; held 30 s after a touch), applied by `ui.c` `follow_hosts`; past the last computer is the connection page (`ui.c` `show_info`: the screen's address and pairing code, never in the turns, gone 2 min after a touch or when a computer joins); the rules are in `docs/protocol-v1.md` §7.1 «多台电脑» and `firmware/sim` (`two_computers`) checks them. go-dock sends `settings` on connecting only when they are the user's own (`Options.OwnSettings`) or changed while the screen was away, so a computer left at the defaults does not undo another's. Setting the WiFi waits for the screen's answer (`Manager.ConfigureWiFi`, `WiFiConfig.answeredBy`: a `warn` log is a refusal, the `net` in state `connecting` for the new network the acceptance) and numbers each accepted try (`NetStatus.Try`); the web dialog (`app.js` `joinWiFi`, `joinOutcome`) reports the outcome only from a `usb.net` with that number, so news about the WiFi as it was are never taken for it. `handle_wifi` in `link.c` must keep answering that way.

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
# with "device":{"enabled":true} it opens the screen's USB cable too: only setup and updates go over it
echo '{"hook_event_name":"PreToolUse","session_id":"s","cwd":"/w/p","tool_name":"Bash","tool_input":{"command":"ls"}}' \
  | ./bin/go-dock hook claude --addr 127.0.0.1:47399
curl -s http://127.0.0.1:47399/api/state   # snapshot + the exact device frame
```

Never install integrations into the real `~/.claude/settings.json` or `~/.codex/hooks.json` while testing; point `CLAUDE_CONFIG_DIR` / `CODEX_HOME` at a temp dir.


Firmware UI checks without hardware: `firmware/sim` compiles `main/ui.c` against LVGL on the desktop and writes PPM screenshots. It needs `idf.py reconfigure` once (downloads LVGL into `managed_components/`) and the cmake from the activated ESP-IDF (`. ~/.espressif/tools/activate_idf_v6.1.sh`). Sample scenarios live in `firmware/sim/sim.c`.

## Claude Code data sources (undocumented, parse defensively)

- `~/.claude/sessions/<pid>.json`: live status (`busy|idle|waiting`, `waitingFor`); a file whose PID is no longer alive means the session ended.
- `~/.claude/projects/**/<session>.jsonl`: token usage (deduplicated per `message.id`, max per field), `ai-title` / `custom-title` lines for session titles (a `/rename` title wins), the last assistant text block for `Session.Message` (cleared by the next user prompt); `subagents/agent-<id>.jsonl` + `.meta.json` for subagents without hooks.
- statusLine stdin `rate_limits` is the only source of Claude 5h/7d percentages; go-dock wraps the user's existing statusLine command (`statusline --chain <base64 of original>`). Every open session runs it, an idle one with the limits of its last API reply, so the last report is not always the newest: `claude.limitTracker` remembers each session's last report and counts one as fresh only when that session got a reply since (`cost.total_api_duration_ms` grew, or the window changed). A fresh report wins even when lower (users can reset their usage within a window); until one arrives (after go-dock starts) `claude.pick` keeps the later window and, within one window (same `resets_at`), the higher number. An older window never wins.

## Codex data sources (undocumented, parse defensively)

- `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl`, one file per thread (subagents too, linked by `session_id` / `parent_thread_id`). Since Codex 0.147 threads are `"history_mode":"paginated"`: no `user_message` / `agent_message` events, messages come as `item_completed` items (`UserMessage`, `AgentMessage`). Tool calls are `response_item` lines (`function_call` with `arguments` as JSON text, `custom_tool_call` for `apply_patch`), written before the tool runs. Approval requests are never written: only hooks report them.
- `$CODEX_HOME/session_index.jsonl`: thread names (`/rename` or generated after the first prompt), appended; the last non-empty name per `id` wins.
- `$CODEX_HOME/thread-writer-locks/<id>.lock` exists while a thread is open (0.155+); older versions fall back to file mtime.

## Releases

Pushing `vX.Y.Z` publishes a release; a tag with a suffix (`v0.1.2-beta.2`) is published as a prerelease and only offered to users who enabled "接收测试版". The release also attaches the firmware, built with the tag as its version: `go-dock-firmware-v2.bin` (merged image, flash at `0x0` by hand) and `go-dock-firmware-v2-app.bin` (app only).

Firmware updates (`docs/protocol-v1.md` §3.4): `app.ApplyFirmware` → `update.Updater.Firmware` downloads the app image of go-dock's own version → `device.Manager.StartFlash` checks it and hands it to the serve loop of the link it goes over (USB when plugged in, else WiFi: `app.upgradeRoute`), which streams it chunk by chunk (no snapshots meanwhile) → `firmware/main/ota.c` writes the spare OTA slot and restarts → the manager waits for a hello with the new version (over WiFi on the next link). Over WiFi the device first answers `confirm` and asks on the screen (`ota_ask`, the 取消/升级 buttons in `ui.c` `build_overlay`); `link.c` `net_ota` carries on once the user answered (`confirmed`, then `ready`) and keeps the link alive meanwhile. Progress is `device.Status.Flash` (`via`, state `confirming`); the web banner reads it from `state.device`.
