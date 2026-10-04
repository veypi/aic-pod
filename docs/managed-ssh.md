# 托管 SSH

host 的 vsh 平台命令提供 `ssh`、`scp`、`sftp`。cloud/page 不注册这些能力。
三者使用系统 OpenSSH 客户端完成认证和加密；文件传输通过
`ssh -s sftp` 的二进制管道和 `github.com/pkg/sftp` 完成，本地文件由 pod 的
会话文件系统访问，不启动原生 scp/sftp 读取宿主文件。

## 使用

```sh
commands
ssh --help
grant ssh server.example.com:22
ssh deploy@server.example.com 'uname -a'
scp ./artifact.tar deploy@server.example.com:/srv/incoming/artifact.tar
scp -r deploy@server.example.com:/srv/reports ./reports
sftp -b ./transfer.batch deploy@server.example.com
```

`grant` 仍需现有发送前审批；SSH 命令不新增审批类别。主机密钥应由设备所有者
预先核实并写入 `~/.ssh/known_hosts`。连接授权不等于接受未知主机密钥。
加密私钥须事先在设备 agent 中解锁；托管调用不会询问密码或私钥口令。

| 命令 | 参数与语义 |
|---|---|
| `ssh target command...` | 必须有远端命令；不接受 SSH 选项。多个命令参数按空格连接为远端 shell 字符串，推荐整体引用。stdin/stdout/stderr 原始字节透传，无 PTY |
| `scp [-r] [-q] [-P port] src dst` | 恰好一端为 `[user@]host:path`；一次一个源与目标；目标是已有目录时追加源 basename；`-r` 递归 |
| `sftp [-q] [-P port] -b file\|- target` | 批模式；`-` 从 stdin 读取，文件通过 fs 域打开；完整解析后建连；最多 1 MiB、4096 条操作 |

SSH/SFTP 目标支持 `user@host:port` 和 `user@[IPv6]:port`。SCP 端口用 `-P`，
IPv6 远端操作数写成 `user@[IPv6]:path`。选项结束标记 `--` 仅用于 scp/sftp。
本地相对路径相对于调用的 cwd，Windows 推荐 `/c/...` 规范形。
远端路径是 SFTP 字面路径：相对服务器起始目录或绝对路径，不展开 `~`、glob
或远端 shell 表达式。

Windows **远端**绝对路径在 scp 和 sftp 中统一支持 `C:/Users/v/x` 与
`/C:/Users/v/x`，传输层统一使用后者；全部使用正斜杠。盘符不依赖本地操作系统，
macOS/Linux 客户端也按同一规则处理。`C:relative` 这种盘符相对路径不支持。
`/c/...` 只属于 Windows 本地文件系统的规范形，不会自动转换成远端 `C:`。
例如 `scp ./artifact.zip win:C:/Users/v/artifact.zip`，对应批命令
`put ./artifact.zip C:/Users/v/artifact.zip`。`cd C:/Users/v` 后可以用相对路径，
`pwd` 显示 `/C:/Users/v`。POSIX 远端若需要名为 `C:` 的相对目录，使用 `./C:/x`。

批文件示例：

```text
put "local file.txt" /srv/incoming/file.txt
get /srv/reports/report.txt ./report.txt
get -r /srv/reports ./reports
ls /srv/incoming
stat /srv/incoming/file.txt
quit
```

支持 `get/put [-r] source destination`、`ls [path]`、`pwd`、`cd path`、
`stat path`、`mkdir path`、`rmdir path`、`rm path`、`rename old new`、`quit`。
所有操作遇错终止；支持引号和行注释，不支持 shell 展开。
不支持本地命令/`!`、glob、`mget/mput`、续传、`-`/`@` 命令前缀、权限保留
选项或任意原生客户端选项。`-q` 接受但不影响 `ls/stat/pwd` 输出，本实现不显示进度条。

## 静态设备配置

默认将 `~/.ssh/config` 当作数据解析，绝不调用 `ssh -G`。
如果 `~/.aic/ssh_config` 存在，则它完全替代默认文件，便于为使用复杂 OpenSSH
配置的设备提供静态连接信息；不读取系统 ssh_config。
覆盖文件不会回退或合并默认配置中的其他别名，因此应包含所有需要的别名。

```text
Host production
  HostName server.example.com
  User deploy
  Port 2222
  IdentityFile ~/.ssh/id_ed25519
  IdentitiesOnly yes
```

支持 `Host`（通配与否定模式）、`HostName`、`User`、`Port`、`IdentityFile`、
`IdentitiesOnly`，标量首值生效，IdentityFile 累加。调用中的 user/port 优先，
冲突的显式端口被拒绝。身份文件允许 `~/`、`%%/%d/%h/%r/%p`，展开后须为绝对路径。
不传脚本的 HOME、PATH、SSH_AUTH_SOCK 等环境给 SSH；agent 来自 pod 设备环境。

`Include` 只接受静态文件名，支持 `~/` 和绝对路径；相对路径按入口配置所在目录
解析（嵌套 Include 也使用同一基准）。最多 8 层、合计 1 MiB，拒绝循环、非普通
文件、glob、环境变量、`%` token 与 `~otheruser` 展开。错误包含来源行号、目标
路径及失败原因。

普通 `~/.ssh/config` 可显式包含目录外的文件，例如
`Include ~/.orbstack/ssh/config`。这意味着设备所有者将配置读取信任传递给这些
文件及其 symlink 目标；应确保它们不由不可信脚本控制。所有文件仍只做静态解析，
不执行其中的命令。OrbStack 的 `Host orb` 不影响其他别名；实际连接 `orb` 时，
其生效的非支持指令仍会报错，不会悄悄丢弃代理。

设备覆盖配置的 Include 仍限于 `~/.aic/ssh/`，例如 `Include ssh/production.conf`，
不可包含 sessions 文件或通过 symlink 越界。`~/.aic/ssh_config`、`~/.aic/ssh/**`
以及 OrbStack 的 `~/.orbstack/ssh/**` 有默认 fs deny，仍遵守显式配置/已审批
grant 优先的现有规则。

`Match` 一律拒绝；生效块内其他不支持的指令（包括 ProxyJump、ProxyCommand、
KnownHostsCommand 等）显式报错。不会删除不支持的跳板配置后静默改为直连。

## 授权与执行边界

1. 解析并冻结最终 host、port、user 和身份文件引用。
2. 按最终规范化 `host:port` 检查 ssh 域；拒绝时展示别名、目标和 `grant ssh` 指引。
3. 对已知本地传输路径预检 fs 域，执行中每个文件操作再次通过同一规则源。
4. 固定系统 SSH 路径，`-F none`，设备最小环境，内部 `NoSandbox=true`。

固定选项关闭代理、TCP/agent/X11/tunnel 转发、本地命令、已知主机命令、连接复用、
自行后台化和交互认证；仅 publickey。连接超时 10 秒。主机校验固定为
`StrictHostKeyChecking=yes`，使用用户 known_hosts，不自动添加/更新主机密钥，
不读取全局 known_hosts。未知或变化的主机密钥都导致失败。

ssh 域范围仍为 host+port，不限制远端 user、命令或远端路径。域名规则表达域名
身份，不锁定 DNS 解析 IP。已建立连接不因后续规则变化自动断开，需 cancel/bg kill。

直接原生执行按 basename 拒绝 `ssh/scp/sftp/slogin/ssh-copy-id/ssh-keyscan`，
不受 exec allow 或 nosandbox 豁免。`grant cmd` 对这些名字提示改用 ssh 域。
vsh 内建 env/xargs/timeout/bash 的子执行回到引擎；外部原生解释器、Git/rsync
子进程及改名程序不受这道名称门的完整约束。本功能不提供全设备 SSH egress 隔离。
`ssh-keygen/ssh-add/ssh-agent` 继续受 exec 域管理。

## 文件与生命周期

本地访问走 `inv.FS`，沿用 fs 规则和写路径记录。host 适配器提供真实 Lstat、
O_EXCL 和目录句柄操作：解析并逐级固定父目录，在打开文件后核对身份，确认前
不读取内容或截断。替换和临时文件删除使用已固定的父目录。规则仍是路径规则，
不宣称防御任意同用户原生进程对整个文件系统命名空间的并发重排或硬链接别名。

上传读取普通文件；下载写同目录随机 `.aic-sftp-*.part`（0600），成功关闭后
原子替换目标。临时路径和最终路径分别授权，单独授予目标文件的权限可能不足，
需要可写的目标目录。失败/取消不提交当前未完成文件，清理失败在 stderr 标出；
已完成文件保留。上传失败可能在远端留下部分文件，目录传输不是整批事务。
不保留远端权限/时间戳。

递归拒绝 symlink、设备、FIFO、socket 以及不安全目录项；最多 64 层、100000 项，
每个文件遵守会话 MaxFileBytes（如已设置）。传输流式进行，不把完整文件放入内存。
远端服务仍是不可信数据源，本地路径构造不接受 `..`、路径分隔符或 Windows 设备名。

vbox RawOutput 关闭 Windows 文本转码/行缓冲，保留字节及写错误，调用方拥有流的
关闭权。SFTP stdout 仅供协议客户端读取，不进入文本日志。SSH stderr 与命令
输出仍走现有执行日志，不新增独立审计系统。

所有进程/协议管道共享执行 ctx；取消关闭管道并回收本地进程。正常子系统关闭
最多等待 5 秒。前台等待到期按已有 NATS/RTC 规则处理，30 分钟默认运行墙钟
到期取消；不承诺清理远端自行脱离会话的后台任务。

用法错误退出 2，权限拒绝 126；ssh 透传客户端退出码（含连接失败 255）；
scp/sftp 操作失败退出 1。外层取消/墙钟超时保持现有 130/124 契约。

## 验证

host 测试包含静态解析、配置副作用拒绝、Include、IPv6、批语法、会话隔离、
本地端口上的真实 OpenSSH 客户端与测试 SSH/SFTP 服务、未知主机密钥、原生名称门、
内建包装命令、二进制往返、目录内 deny、临时文件清理、握手取消，以及文件/父目录
在检查后的替换。vbox 测试覆盖无换行二进制握手、流所有权与写错误传播。
Linux/Windows 需要在目标平台运行测试；跨平台编译不等价于运行验证。
另有 OrbStack 外部 Include 兼容性回归，以及模拟 Windows 盘符目录的 SFTP 协议
服务，覆盖两种绝对路径写法、相对路径、跨盘 cd、rename、递归及二进制往返。
