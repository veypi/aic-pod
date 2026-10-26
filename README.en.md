# aic-pod

**Put a real machine behind your model.**

[![Release](https://img.shields.io/github/v/release/veypi/aic-pod?color=blue)](https://github.com/veypi/aic-pod/releases)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.27-00ADD8)](go.mod)
[![Platform](https://img.shields.io/badge/platform-macOS%20%7C%20Windows%20%7C%20Linux-lightgrey)](#)

<!-- Demo asset slot: drop your 30s GIF on this line -->

aic-pod is the device-side client of the AIC platform: it connects out over NATS WebSocket
and registers the host's abilities — command execution, file operations, browser automation,
native GUI automation, ssh/scp forwarding — as tools an LLM can call. **It listens on no
local port.**

It lets a model *act* on your own machine — really run commands, really read and write files,
really drive a browser and native UI. That is why a large share of this codebase is about
keeping that power on a leash:

- **Outbound only** — no inbound listener, no public IP, no firewall holes.
- **Sandbox that fails closed** — macOS Seatbelt / Linux bubblewrap / Windows restricted token; with no usable backend the command is refused, never run bare.
- **Uniform exec admission + command policies + human approval** — every registered command passes through `exec_policy` before its implementation applies its own FS/NET/SSH policy; a no-sandbox request escalates to separate human approval.

[中文](README.md) | **English**

| Client | Form | Capabilities |
| --- | --- | --- |
| **desktop** (main product) | Electron shell + Go backend child process | exec (uniform admission + command policies + native sandbox), fs, browser, cua (native GUI automation) |
| **cli** | single binary `aic` | exec (uniform admission + command policies + native sandbox, incl. browser/cua/ssh/scp), fs |

Device `fs`/`exec` calls go straight to the executor and file service; standalone services are
reached over [MCP connections](docs/reference/hosts-tools.md). Skills are static cloud content;
browser/CUA are built into the Pod — registered by default and started on demand.

This document describes the current unified command and policy model dated 2026-10-26.
The core migration is complete; caching, the darwin proxy and real-platform validation are
tracked in [docs/development/todo.md](docs/development/todo.md).

The browser runs a real Chrome in the background with a consistent UA, Client Hints,
automation markers and window size, and reuses a dedicated profile; details in
[Browser runtime](docs/guides/browser.md).

## Quick start

```bash
# 1) Get a binding credential (KEY, shown once) from https://ivec.ai
# 2) Download the client for your platform from Releases (desktop app or CLI binary)
aic -key "<your-key>"      # CLI: connect and start taking tasks
```

```bash
# Or turn a server into an execution node with Docker
docker run -d --name aic-pod -e KEY="<your-key>" veypi/aic-pod:latest
```

## Further reading

- Documentation map: [docs/index.md](docs/index.md)
- Security model: [docs/reference/host-sandbox.md](docs/reference/host-sandbox.md)
- Architecture: [docs/architecture/overview.md](docs/architecture/overview.md) (current unified command model, 2026-10-26; remaining work in [docs/development/todo.md](docs/development/todo.md))
- Tools and MCP: [docs/reference/hosts-tools.md](docs/reference/hosts-tools.md)
- Browser/CUA runtime: [docs/guides/browser.md](docs/guides/browser.md)
- Managed SSH / git: [docs/guides/managed-ssh.md](docs/guides/managed-ssh.md), [docs/guides/managed-git.md](docs/guides/managed-git.md)
- Build and release: [docs/guides/release.md](docs/guides/release.md)
- Release history: [CHANGELOG.md](CHANGELOG.md)

## Configuration

The CLI and the desktop client share one config file: `os.UserConfigDir()/aic/config.yaml`
(on macOS `~/Library/Application Support/aic/config.yaml`). Either side can change it — by
editing the file or from the settings window — and the other side picks it up on restart.

Parsing is handled by `vigo/flags`: `AutoRegister` declares fields, `ConfigFile` declares the
file, `Parse` merges everything, precedence **explicit flag > environment variable > config
file > struct default**.

Reading is tolerant: unknown fields are ignored, a bad field falls back to its default while
the rest are kept, and if the file is unreadable or the YAML is broken the client starts on
defaults and you can still repair it with `aic config get|set`. Reading never rewrites the
file — only saving does. The settings surface (`aic config`) shares `flags`' parsing and
atomic writes and only adds business validation of device parameters.

| Environment variable | CLI flag | Config key | Default | Meaning |
| --- | --- | --- | --- | --- |
| `KEY` | `-key` | `key` | | Binding credential (required); obtained from the AIC platform |
| `HOST` | `-host` | `host` | `https://ivec-ai.com` | Platform endpoint (may carry a path prefix, e.g. `http://127.0.0.1:4000/rses/aiv`; the NATS endpoint is derived from it) |
| `WORK_DIR` | `-work_dir` | `work_dir` | system temp dir | Working directory for command execution |
| `EXEC_TIMEOUT` | `-exec_timeout` | `exec_timeout` | `30m` | Background execution timeout |
| `HOME_PATH` | `-home_path` | `home_path` | `/` | Default page the desktop client opens (path after the host, e.g. `/`, `/a`) |
| `EXEC_POLICY` | `-exec_policy` | `exec_policy` | `open` | Uniform admission stance for every registered command; a denial prevents the implementation from being called |
| `FS_POLICY` | `-fs_policy` | `fs_policy` | `deny` | Default file-write stance for vbox-backed commands: `deny` (built-in roots + `fs_allow` only) or `open` (everything except `fs_deny`) |
| `FS_DENY` / `FS_ALLOW` | `-fs_deny` / `-fs_allow` | `fs_deny` / `fs_allow` | — | Path globs to deny / explicitly allow (an allow can override a deny; a bare path covers its subtree, globs match exactly) |
| `NET_POLICY` | `-net_policy` | `net_policy` | `open` | Network stance for vbox-backed commands and the Pod `curl` command: `open` or `deny`; each path enforces it independently without `tool:` |
| `NET_DENY` / `NET_ALLOW` | `-net_deny` / `-net_allow` | `net_deny` / `net_allow` | — | Egress targets `host:port` to deny / allow (target matching uses level + original order; `localhost:*` is built in) |
| `NET_UPSTREAM_PROXY` | `-net_upstream_proxy` | `net_upstream_proxy` | — | Explicit `http://`, `socks5://` or `socks5h://` upstream for the darwin deny egress proxy; included in netKey, never inherited from host proxy env |
| `SSH_POLICY` | `-ssh_policy` | `ssh_policy` | `deny` | Target stance for Pod `ssh/scp/sftp` commands: `deny` or `open`; does not constrain `/bin/ssh` |
| `SSH_DENY` / `SSH_ALLOW` | `-ssh_deny` / `-ssh_allow` | `ssh_deny` / `ssh_allow` | — | Pod SSH command targets `host[:port]` to deny / allow; see the permission doc for matching semantics |
| `NO_SANDBOX` | `-no_sandbox` | `no_sandbox` | `false` | Hidden: skip the exec sandbox globally (equivalent to giving up process-level isolation; use with care; config file/flag/env only) |

> Uniform exec admission, fs/net/ssh configuration ownership, built-in roots and session-scoped
> temporary grants are documented in [docs/reference/host-sandbox.md](docs/reference/host-sandbox.md).

Nothing is exposed on a local port (the local management API was removed on 2026-09-22): the
settings surface *is* `UserConfigDir/aic/config.yaml`, and the CLI reads and writes it through
`aic config get|set` / `aic bind|unbind` (JSON and credentials over stdin/stdout). The desktop
settings window spawns the same subcommands over Electron IPC. Saving requires a backend
restart to take effect.

## Security model

Host capabilities are separated into three boundaries:

- **Uniform command admission**: every registered command passes through vsh's `exec_policy`;
  a denial prevents the implementation from being called.
- **Command-owned policies**: vbox-backed commands use FS/NET, Pod `curl` uses NET, and Pod SSH
  commands use SSH. Built-in roots cover the workspace, session directory and system temp;
  session grants can be made permanent.
- **Native-process sandbox**: vbox-backed commands use macOS Seatbelt / Linux bubblewrap /
  Windows restricted tokens to enforce FS/NET, with environment scrubbing and resource limits.
  With no usable backend or an unsupported policy, the dependent command **fails closed** — no
  dropped rules, rerouting or unrelated command outage.

A request may ask for `nosandbox` only through explicit human approval; an ordinary approval
does not itself waive the sandbox. Uniform exec admission and command-owned policies remain
in force, with no SSH/Git-specific exception.

## CLI

### Install

Download the binary for your platform from [Releases](https://github.com/veypi/aic-pod/releases) and put it in `PATH`.

### Usage

```bash
aic                                  # connect and run (auto-connects when config.yaml has a key)
aic config get | config set          # read/write local settings (JSON over stdin/stdout)
aic bind | unbind                    # bind/unbind platform credentials (credentials over stdin)
aic -key "<key>"                     # temporary override
# all flags: aic -h
```

Temporary flags (`-host` / `-key` / `-work_dir` / `-exec_timeout` / `-home_path`, or the matching
environment variables) affect only that run; **to make something permanent edit
`UserConfigDir/aic/config.yaml`** (or use `aic config set` / the desktop settings window —
restart the backend afterwards).

### Running in the background (macOS/Linux)

```bash
nohup aic > aic.log 2>&1 &
```

## Docker

### Build and push

```bash
make docker-build        # build linux/amd64 and the image → veypi/aic-pod:latest
make docker-build-arm64  # build linux/arm64 and the image
make docker-push         # push to Docker Hub
```

### Run

```bash
# minimal
docker run -d --name aic-pod -e KEY="<key>" veypi/aic-pod:latest

# full
docker run -d \
  --name aic-pod \
  --restart unless-stopped \
  -e KEY="<key>" \
  -e WORK_DIR=/workspace \
  -e EXEC_TIMEOUT=30m \
  -e HOST=https://ivec-ai.com \
  -v /host/workspace:/workspace \
  veypi/aic-pod:latest
```

| Docker option | Meaning |
| --- | --- |
| `--restart unless-stopped` | restart the container automatically after exit |
| `-v /host:/workspace` | mount a host directory as the command working directory |
| `-e KEY` | required; the binding credential from the AIC platform |

### Logs

```bash
docker logs -f aic-pod
```

## Browser / CUA

browser and cua start the official `agent-browser` 0.38.2 and `cua-driver` 0.33.2 in MCP mode
directly; they start lazily on first call and share the connection afterwards. All tool names,
parameters and results follow upstream. The desktop build ships Chrome and both official native
programs; the standalone CLI installs what it needs at runtime as described by each skill.

```sh
mcp tools browser
mcp call browser new_page --input '{"url":"https://example.com"}'
mcp call browser take_snapshot --input '{"pageId":1}'
mcp tools cua
```

## Build

A plain `go build ./cli` / `go run ./cli` includes native execution, the file service and the
MCP manager. Upstream tools run as separate processes; the desktop packaging ships pinned
versions. Cloud skill content lives in aic-skills.

Artifacts: desktop is the main product (`aic-*`), the CLI is `aic-cli-*`.

```bash
make                            # cli for the current platform → dist/aic-cli-<os>-<arch>
make cli-all                    # cli for all platforms (linux/darwin/windows × amd64/arm64)
make desktop-darwin-arm64       # desktop for macOS → dist/AIC Desktop.app + aic-desktop-darwin-arm64.dmg
make desktop-darwin-amd64       # desktop for macOS (Intel)
make desktop-windows-amd64      # desktop for Windows (needs: brew install mingw-w64)
make desktop-all                # desktop for all platforms (the Linux desktop build needs a container/CI)
make docker-build               # build the cli and the Docker image
make docker-push                # push the image
make release                    # full local build + gh release create (needs every platform toolchain)
make clean                      # clean up
```

Before packaging, the desktop build syncs the bundled cua-driver (`desktop/cua.json` pins the
version and sha256 → `vendor/cua → resources/cua`, three platforms). On an offline or
restricted network use `npm run cua-sync -- --asset <downloaded asset>`.

**Release flow**: pushing a `v*` tag triggers CI (`.github/workflows/build.yml`), which builds
the desktop and CLI for every platform and creates a GitHub Release. The version number lives
in `cfg/config.go` only (with a `v` prefix); `desktop/package.json` is synced automatically by
`make desktop-version` from `git describe`.

Requirements: Node 22+ (Electron/electron-builder), Go (backend binary via `make backend-bin`),
`go-winres` (Windows CLI resources).

## License

MIT — see [LICENSE](LICENSE).
