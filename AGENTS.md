# Repository Guidelines

Notes for AI agents and contributors working **in this repository**. The user-facing overview
is [README.md](README.md) (Chinese) / [README.en.md](README.en.md) (English).

## What this repo is, and what it is not

The **device-side client** of the AIC platform: it connects out over NATS WebSocket / WebRTC and
exposes the host's capabilities (exec, fs, browser, cua, ssh/scp) as tools an LLM can call.

This repo owns: sandbox and authorization enforcement, the exec/fs tool layer, the transport
contracts, the desktop packaging and the CLI. It does **not** own: the platform side (accounts,
approvals, prompts), or skills — skills are static cloud content and the in-repo skill/package
mechanism (`libs/skillrun`, `libs/uiscript`, `protocol/hosts_tools`, any aic-skills dependency) was
removed on purpose; do not reintroduce it.

## Layout

- `cli/` — the single binary `aic` (`go build ./cli`), including the `config` / `bind` / `unbind`
  settings surface.
- `desktop/` — Electron shell with the Go backend as a child process, plus the settings UI and
  packaging (`electron-builder.yml`, `agent-browser.json`, `cua.json`). `node_modules/` and
  `vendor/` are build artifacts: do not scan, link or "fix" them.
- `cfg/` — config schema/defaults and `Version` (the only place the version number lives).
- `libs/` — `execution` (exec runner), `fsx` (fs tool layer), `host` (host engine: connect,
  dispatch, deny rules, browser stream, vsh engine), `hostfs` (os.Root-backed backend and path
  canonicalization), `mcpx` (official MCP Go SDK wrapper), `rtc`, `imageutil`.
- `protocol/` — transport-independent contracts: `tool.go` (the request/action face), `caps.go`,
  `fs.go`, `hosts_nats.go`, `hosts_rtc.go`, `path.go`, `sign.go`, `subject.go`, RTC signal/ticket.
- `docs/` — Chinese design and operational docs: `host_sandbox.md` (security/authorization
  contract), `hosts-tools.md` (tool and MCP surface), `design.md`, `managed-ssh.md`,
  `release-architecture.md`.
- `Makefile`, `Dockerfile`, `resources/` (icons + `winres.json`), `desktop/scripts/` (packaging hooks), `dist/` (build output).

## Build & test

```sh
go build ./... && go vet ./... && go test ./...
go build ./cli ; go run ./cli

make             # cli for the current platform → dist/aic-cli-<os>-<arch>
make cli-all     # cli for every platform
make backend-bin ; make desktop-deps ; make desktop-darwin-arm64 ; make desktop-windows-amd64
make docker-build ; make docker-push ; make release ; make clean ; make help
```

- Go 1.27 (see `go.mod`). The desktop build needs Node 22+; Windows CLI resources need `go-winres`
  (`cli/rsrc_windows_amd64.syso`); the Windows desktop build needs `mingw-w64`
  (`brew install mingw-w64`).
- Sandbox behavior needs real backends: Seatbelt on macOS, bubblewrap + usable user namespaces on
  Linux, a real token/handles on Windows. Cross-compiling is not verification — write "not
  verified" instead of "passes".
- SSH integration cases need `nosandbox`: the sandbox blocks the loopback listener they rely on.
- **Fixture names must avoid the built-in credential deny patterns** (`*.pem`, `.env`, `id_rsa*`,
  `.ssh/**`, …). The exec sandbox denies writes to those paths, so such a case fails with EPERM and
  looks like a product bug. This bites whenever a test writes a `.env`-style temp file.
- Desktop shell tests: `node --test desktop/*.test.cjs` (`host-state`, `backend-startup`).

## Invariants (do not break without an explicit decision)

- **Sandbox fails closed**: with no usable backend the command is not run — never run bare.
- **`nosandbox` is request-level and separately approved**: it always escalates to Critical(4) and
  an approval does not by itself waive the sandbox.
- **Three-domain authorization** (fs/net/ssh × policy/deny/allow): deny always wins, an explicit
  allow may override a deny, built-in roots are workspace / session dir / system temp, and session
  grants (`grant <domain> <target> [--permanent]`) are the only ad-hoc widening. Changing the
  decision formula means updating `docs/host_sandbox.md` in the same turn.
- **No local listening port**: the settings surface is the shared config file plus
  `aic config`/`aic bind` (the desktop window spawns the same subcommands over Electron IPC). Do
  not reintroduce a local management API.
- **Config semantics**: precedence flag > env > config file > struct default; tolerant parse (unknown
  fields ignored, bad field falls back to its default); reads never rewrite the file; writes only
  when saving; a broken file still starts on defaults.
- **Protocol contracts are transport-independent and versioned** (`hosts_nats/3`, `hosts_rtc/3`):
  wire changes bump the version, and `protocol/tool` `Request{protocol, request_id, action,
  exec|cancel_id|fs}` stays the single action face — exec carries the whole script only, native fs
  goes through the same face.
- **The version number lives only in `cfg/config.go`** (with a `v` prefix). `desktop/package.json`
  is synced by `make desktop-version` from `git describe`; pushing a `v*` tag triggers the release
  workflow.
- **browser/cua are upstream programs** (`agent-browser`, `cua-driver`, pinned in `desktop/*.json`):
  tool names, parameters and results pass through unchanged, and the Pod only starts/streams them.
  Do not hand-roll Chrome/CDP again.
- **MCP services are lazily started and shared**, and a service with `idle_timeout` is released
  after that much idleness (in-flight requests count as activity; a call renews it) — release is a
  control action and must not log as a failure. The built-in browser entry defaults to 30m; its
  upstream daemon is closed explicitly when the entry was idle-released.
- `libs/hostfs` echoes caller-facing paths and stable error kinds (`not_found`,
  `permission_denied`, `filesystem_error`) — keep errors actionable rather than collapsing
  everything into `internal`.

## Docs & language

- `README.md` is **Chinese** and is the repo's main, human-facing entry point; `README.en.md` is
  the English mirror. Both open with a language switcher line and must be updated together. Keep
  editorial detail (repo map, verification workflow, invariants) in this file instead of the
  READMEs.
- `docs/*.md` stay Chinese: they are the normative design/security docs, and the READMEs link to
  them directly.
- `CHANGELOG.md`: one section per version (`## 未发布` while unreleased), breaking changes first,
  and note anything that requires a device restart (e.g. backend-only changes need the desktop shell
  rebuilt via `make backend-bin`).

## Commits & releases

Short imperative subjects scoped to one change. To release: bump `cfg/config.go`'s `Version`, add
the CHANGELOG section, then push a `v*` tag — CI builds the desktop and CLI for every platform and
creates the GitHub Release.
