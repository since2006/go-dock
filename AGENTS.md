# AGENTS.md

Guidance for AI coding agents working in this repository.

## Layout

- `host/` — Go module `github.com/since2006/go-dock/host` (the desktop daemon).
  - `cmd/go-dock/` — CLI: `run` (default, `--background` for login items), `hook <claude|codex>`, `statusline`, `autostart on|off|status`, `version`.
  - `internal/model/` — shared types (`Session`, `State`, `Usage`, `Snapshot`, `Observation`).
  - `internal/hub/` — merges observations into ordered sessions + usage; the single source of truth.
  - `internal/providers/` — collectors. `claude/` and `codex/` each have: status-file / log polling, hook mapping, install/uninstall of integrations. `jsonl/` is the incremental JSONL reader, `pricing/` the cost table. Shared install helpers live in `providers/install.go`.
  - `internal/device/` — USB serial link to the screen (`protocol.go` mirrors `docs/protocol-v1.md`).
  - `internal/mqtt/` — MQTT connections for plugins: `Client` is one broker (own reconnect loop, subscriptions kept across reconnects, fail-fast publish), `Pool` keeps one `Client` per configured broker id; `mqtttest/` is an in-process broker for tests.
  - `internal/plugins/` — function pages next to the Agent page: the `Plugin` interface and `Host`; one subpackage per plugin (`lights/` over MQTT, `aircon/` a Gree unit over the screen's infrared, `screen/` the screen's own brightness and muting the alerts, saved to the config). Plugins describe pages with the generic widgets in `model.Page` (`switch`, `stepper`, `select`), reach MQTT through brokers they name by id and infrared through `device.Manager.SendIR`, and never talk to the device or UI directly.
  - `internal/ir/` — infrared codecs as raw µs timings (Gree, after IRremoteESP8266); the screen only sends and receives the timings.
  - `internal/web/` — HTTP API + embedded UI (`static/`, plain HTML/CSS/JS, no build step).
  - `internal/app/` — wiring; implements `web.Backend`.
  - `internal/autostart/` — login item per OS (LaunchAgent / systemd user unit or XDG / HKCU Run).
  - `internal/update/` — checks GitHub Releases, verifies and replaces the executable; `cmd/go-dock/restart_*.go` restarts it.
  - `internal/config/`, `internal/logx/`, `internal/ingest/`, `internal/procutil/`, `internal/selfexe/` — support packages.
- `firmware/` — ESP-IDF 6.1 + LVGL 9 firmware for the **V2** board only (pins differ from V1). `main/display.c` is the board bring-up, `link.c` the protocol, `ota.c` updates over the link, `ui.c` the screen (the Agent page and the function pages), `beep.c` plays alert notes on an external buzzer (GPIO1; the board has no speaker), `ir.c` sends and receives raw infrared timings through external modules (GPIO7 / GPIO8), `settings.c` keeps the brightness go-dock set in NVS. `sim/` renders `ui.c` on the desktop to PNG-able PPM files for layout checks without hardware.
- `docs/protocol-v1.md` — host ↔ device protocol. `docs/plans/` — implementation plans.

## Commands

```sh
cd host
gofmt -l .                 # must print nothing
go vet ./...
go test -race ./...
CGO_ENABLED=0 GOOS=windows go build ./cmd/go-dock   # also linux, darwin

# firmware (ESP-IDF v6.1, installed by EIM; a git checkout activates with `. ~/esp/esp-idf/export.sh`)
. ~/.espressif/tools/activate_idf_v6.1.sh && cd firmware && idf.py build
cmake -B build-sim -S sim && cmake --build build-sim && build-sim/dock_sim out   # UI screenshots
```

## Rules

- **Protocol changes**: update `docs/protocol-v1.md`, `host/internal/device/protocol.go` and `firmware/main/link.c` (+ `model.h`) together; keep the 4096-byte line cap; only add optional fields unless bumping `proto`. Optional device features (`ota`, `beep`, `pages`, `ir`, `settings`) are announced in the device hello, and the host sends their messages only to devices that announce them.
- **Privacy**: never read or send credentials (`~/.claude/.credentials.json`, Keychain, `~/.codex/auth.json`). Never log hook/statusline payloads (they contain prompts and tool input) above debug level.
- **Editing user config files** (`~/.claude/settings.json`, `~/.codex/hooks.json`): go through `providers.ReadJSONFile` / `Save`, which keep unrelated content and key order, back up the original, and write atomically. `~/.codex/hooks.json` may only contain the top-level keys `description` and `hooks`. Replace our hook entries in place rather than reordering (Codex trust keys include array indices).
- **Hook subcommands** must stay silent and exit 0, even when the daemon is down.
- **Undocumented sources** (`~/.claude/sessions/*.json`, Codex rollout files) can change between versions: parse defensively, every field optional.
- UI text and docs are Chinese; code, identifiers and comments are English.
- **Releases**: pushing a `v*` tag runs `.github/workflows/release.yml`. The updater expects assets named `go-dock_<os>_<arch>[.exe]` plus `SHA256SUMS`, and `go-dock version` printing the tag without `v`; change both sides together. Builds without `-X main.version` report `dev` and never self-update. `GO_DOCK_UPDATE_API` points the updater at a fake GitHub API for testing.
- **Firmware updates**: the firmware version follows the tag (CI writes it into `firmware/version.txt`; local builds say `dev`), and go-dock offers the device the `go-dock-firmware-v2-app.bin` of its own release. Keep that asset name, the image checks in `device.CheckImage` and the OTA partition layout in `firmware/partitions.csv` in step. The partition table and bootloader cannot change over the link, only by flashing the merged image by hand. `GO_DOCK_FIRMWARE_FILE=<app image>` offers a local build instead, for testing on hardware.
- **Autostart** never starts a second instance immediately (the running one owns the port); it only registers for the next login.
- New behavior needs tests; providers keep fixtures in their `testdata/`.
