# aic-pod

**Put a real machine behind your model.** aic-pod is the device-side client of the AIC
platform: it connects out over NATS WebSocket and registers the host's abilities — command
execution, file operations, browser automation, native GUI automation, ssh/scp forwarding —
as tools an LLM can call. It listens on no local port.

[中文](README.md) | **English**

| Client | Form | Capabilities |
| --- | --- | --- |
| **desktop** (main product) | Electron shell + Go backend child process | exec (sandboxed, three-domain authorization), fs, browser, cua (native GUI automation) |
| **cli** | single binary `aic` | exec (sandboxed, three-domain authorization, incl. browser/cua/ssh/scp), fs |

Device `fs`/`exec` calls go straight to the executor and file service; standalone services are
reached over [MCP connections](docs/hosts-tools.md). Skills are static cloud content;
browser/CUA are built into the Pod — registered by default and started on demand.

The browser runs a real Chrome in the background with a consistent UA, Client Hints,
automation markers and window size, and reuses a dedicated profile; details in
[Browser runtime](docs/hosts-tools.md#browser-运行环境).

- Security model: [docs/host_sandbox.md](docs/host_sandbox.md)
- Architecture: [docs/design.md](docs/design.md)
- Tools and MCP: [docs/hosts-tools.md](docs/hosts-tools.md)
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
| `FS_POLICY` | `-fs_policy` | `fs_policy` | `deny` | Default file-write stance: `deny` (built-in roots + `fs_allow` only) or `open` (everything except `fs_deny`) |
| `FS_DENY` / `FS_ALLOW` | `-fs_deny` / `-fs_allow` | `fs_deny` / `fs_allow` | — | Path globs to deny / explicitly allow (an allow can override a deny; a bare path covers its subtree, globs match exactly) |
| `NET_POLICY` | `-net_policy` | `net_policy` | `open` | Egress stance of sandboxed processes: `open` or `deny` (localhost only) |
| `NET_DENY` / `NET_ALLOW` | `-net_deny` / `-net_allow` | `net_deny` / `net_allow` | — | Egress targets `host:port` to deny / allow (deny always wins; `localhost:*` is built in) |
| `SSH_POLICY` | `-ssh_policy` | `ssh_policy` | `deny` | ssh/scp target stance: `deny` or `open` |
| `SSH_DENY` / `SSH_ALLOW` | `-ssh_deny` / `-ssh_allow` | `ssh_deny` / `ssh_allow` | — | ssh targets `host[:port]` to deny / allow (deny always wins) |
| `NO_SANDBOX` | `-no_sandbox` | `no_sandbox` | `false` | Hidden: skip the exec sandbox globally (equivalent to giving up process-level isolation; use with care; config file/flag/env only) |

> The full decision rules for the three domains (fs/net/ssh × policy/deny/allow), the built-in
> roots and the session-scoped temporary grants are documented in
> [docs/host_sandbox.md](docs/host_sandbox.md).

Nothing is exposed on a local port (the local management API was removed on 2026-09-22): the
settings surface *is* `UserConfigDir/aic/config.yaml`, and the CLI reads and writes it through
`aic config get|set` / `aic bind|unbind` (JSON and credentials over stdin/stdout). The desktop
settings window spawns the same subcommands over Electron IPC. Saving requires a backend
restart to take effect.

## Security model

Host capabilities sit behind two gates:

- **Three-domain authorization** (fs / net / ssh × policy / deny / allow): each domain has a
  default stance (`deny`/`open`), a deny list and an explicit allow list (deny always wins; an
  explicit allow may override a deny). There are three classes of built-in roots (workspace,
  session directory, system temp) plus session-scoped temporary grants
  (`grant <domain> <target>`, add `--permanent` to persist).
- **Process sandbox** (exec calls): macOS `sandbox-exec` (Seatbelt) / Linux bubblewrap /
  Windows restricted token. The profile (read-only or workspace-write) follows the granted
  level and is combined with environment scrubbing, resource limits and the network egress
  gate. With no usable backend it **fails closed** — the command is not run, never run bare.

A request may ask for `nosandbox` explicitly: it always escalates to Critical(4) and therefore
to human approval, and an approved request does not by itself waive the sandbox — the
no-sandbox flag must carry its own approval.

## CLI

### Install

Download the binary for your platform from [Releases](../../releases) and put it in `PATH`.

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
