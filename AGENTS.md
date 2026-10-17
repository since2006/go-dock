# AGENTS.md

Guidance for AI coding agents working in this repository.

## Layout

- `host/` — Go module `github.com/since2006/go-dock/host` (the desktop daemon).
  - `cmd/go-dock/` — CLI: `run` (default), `hook <claude|codex>`, `statusline`, `version`.
  - `internal/model/` — shared types (`Session`, `State`, `Usage`, `Snapshot`, `Observation`).
  - `internal/hub/` — merges observations into ordered sessions + usage; the single source of truth.
  - `internal/providers/` — collectors. `claude/` and `codex/` each have: status-file / log polling, hook mapping, install/uninstall of integrations. `jsonl/` is the incremental JSONL reader, `pricing/` the cost table. Shared install helpers live in `providers/install.go`.
  - `internal/device/` — USB serial link to the screen (`protocol.go` mirrors `docs/protocol-v1.md`).
  - `internal/web/` — HTTP API + embedded UI (`static/`, plain HTML/CSS/JS, no build step).
  - `internal/app/` — wiring; implements `web.Backend`.
  - `internal/config/`, `internal/logx/`, `internal/ingest/`, `internal/procutil/` — support packages.
- `firmware/` — ESP32 firmware (placeholder for now).
- `docs/protocol-v1.md` — host ↔ device protocol. `docs/plans/` — implementation plans.

## Commands

```sh
cd host
gofmt -l .                 # must print nothing
go vet ./...
go test -race ./...
CGO_ENABLED=0 GOOS=windows go build ./cmd/go-dock   # also linux, darwin
```

## Rules

- **Protocol changes**: update `docs/protocol-v1.md` and `host/internal/device/protocol.go` together; keep the 4096-byte line cap; only add optional fields unless bumping `proto`.
- **Privacy**: never read or send credentials (`~/.claude/.credentials.json`, Keychain, `~/.codex/auth.json`). Never log hook/statusline payloads (they contain prompts and tool input) above debug level.
- **Editing user config files** (`~/.claude/settings.json`, `~/.codex/hooks.json`): go through `providers.ReadJSONFile` / `Save`, which keep unrelated content and key order, back up the original, and write atomically. `~/.codex/hooks.json` may only contain the top-level keys `description` and `hooks`. Replace our hook entries in place rather than reordering (Codex trust keys include array indices).
- **Hook subcommands** must stay silent and exit 0, even when the daemon is down.
- **Undocumented sources** (`~/.claude/sessions/*.json`, Codex rollout files) can change between versions: parse defensively, every field optional.
- UI text and docs are Chinese; code, identifiers and comments are English.
- New behavior needs tests; providers keep fixtures in their `testdata/`.
