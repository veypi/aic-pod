# Host 执行策略与原生沙箱

状态：2026-09-16 当前实现。云端审批策略与本地执行策略分别决策；完整协议见 aic 的 `docs/permission.md`。本地策略不能被云端批准覆盖，不保留旧配置兼容逻辑。

## 配置

```yaml
fs_policy: deny
fs_deny: ["/work/private/**"]
fs_allow: ["/work/project/**", "ro:/public/docs/**"]
exec_policy: deny
exec_deny: ["bash"]
exec_allow: ["git", "json"]
net_policy: deny
net_deny: ["blocked.example:*"]
net_allow: ["example.com:443"]
ssh_policy: deny
ssh_deny: []
ssh_allow: ["dev.example:22"]
```

统一判定顺序为 deny、覆盖本次访问的 allow、policy 默认值。deny 恒优先；open 默认允许，deny 默认拒绝。列表全部是字符串，fs 普通路径授予读写，`ro:` 只授予读取；`ro:C:/docs/**` 保留盘符。裸 fs 路径覆盖子树，支持段内 `*`、跨层 `**` 和 `?`；exec 使用命令名精确匹配或 `*`，不把命令正文作为配置规则。

net/ssh 以 `host:port` 精确匹配主机，端口可为数字或 `*`。IPv6 使用方括号。明确 deny 不能被更具体的 allow 覆盖。

默认 fs/ssh policy 为 deny，exec/net 为 open。工作区、Session 区、临时区、工具缓存装配为默认读写资源；系统运行库、系统 CA 装配为只读资源；默认凭证保护名单与 fs_deny 合并后统一优先。系统运行库不包含整个用户目录或整个 `/System/Volumes/Data`。调用参数 workdir 只决定工作目录，不授予该目录权限。

`cfg.ValidateAuth` 在读取和保存时校验。配置文件仅接受当前 snake_case 键；坏配置不能静默替换正在生效的策略。本地设置 API 与永久 grant 使用同一配置更新锁，写盘成功后更新内存。

## 请求处理

1. 验证签名、deadline、nonce、Session 与工具参数。
2. 检查可信 granted_level 与本地声明所需等级；不足直接返回权限错误。
3. 检查 exec 命令策略及相应 fs/net/ssh 范围。
4. 原生进程根据当次本地策略生成 OS 沙箱；虚拟指令在实际 IO 处检查。

任何本地权限失败都返回 rejected/error 给 AI，host 不返回 waiting 或提权请求。即使 provider 返回 waiting，host 响应出口也转换为拒绝。云端请求中的 granted_level=9 只表示审批完成，不能更改本地 allow/deny。

## grant 与生命周期

```text
grant fs /work/extra --temp
grant exec python3 --temp
grant net example.com:443 --permanent
grant ssh dev.example:22 --temp
```

grant 自身 required=4，先经正常云端审批再修改 host 的对应 allow。省略范围时默认为本 Session 临时授权，永久授权写入本地配置。deny 内目标不能申请；exec grant 必须指定本机已注册命令。commands/grant 默认可用于发现和申请，明确 exec_deny 仍可关闭它们。

临时授权按 Session 隔离，保存在设备进程内存，随进程退出或重启清空；Session clear/delete 不影响临时授权（2026-09-21 定）。永久授权写入设备本地配置，不受 Session 清理影响。运行中的进程继续使用启动时快照；新启动重新读取策略。

浏览器扩展 host 使用相同的 fs/exec 字符串规则，OPFS 根与命令默认全开；支持 fs/exec 临时 grant、通过扩展本地 settings 保存永久 grant。SDK 宿主没有配置保存入口时，永久 grant 报错而不假称成功。扩展没有原生 net/ssh 沙箱能力。

## OS 沙箱

- macOS Seatbelt：默认关闭文件读取和写入，按读/写 allow 生成范围，最后叠加 deny。ro 不进入写允许规则。dyld 需要读取根目录本身及允许路径的祖先元数据；这些是 literal 规则，不放开目录子树。网络规则仅对 loopback 可以精确执行；不能表达的域名/IP 条目在启动前拒绝。
- Linux bubblewrap：关闭策略使用空根和明确绑定的只读/读写目录，不因 workdir 自动绑定目录。bwrap 不能完整表达路径 deny、动态路径 glob 和精确网络目标；存在路径 deny 或其他无法落实的策略时明确返回权限错误。默认配置含凭证 deny，所以当前配置下的原生命令会被拒绝，不能用近似挂载冒充完整保护。
- Windows：保留受限令牌、ACL 和 Job Object 资源限制。该后端没有实现路径读取限制和网络规则，携带这些规则的原生调用在启动前拒绝；虚拟 fs、net/ssh 的本地检查仍工作。

nosandbox 免沙箱执行不再叠加本地 fs/net 策略条件：请求级 nosandbox 经 dispatch 强制 Critical(4) 人工审批（granted 9 随签名下发）后直接执行；ssh/scp 内部管控调用与全局 no_sandbox 配置同属免沙箱来源。无法建立沙箱时仍拒绝运行（沙箱路径 fail-closed），不提供静默裸跑兜底。

资源限额、进程组终止、后台任务与输出处理保持原有实现。Windows 盘符虚拟根仍通过统一路径模块解析。

## 验证

```sh
GOCACHE=/tmp/aic-permission-go-cache go test ./cfg ./libs/policy ./libs/fsauth ./libs/netauth ./libs/vcore ./libs/exec_procs ./libs/host ./api -skip '^TestCuaLive'
AIC_SANDBOX_PROBE=1 GOCACHE=/tmp/aic-permission-go-cache go test ./libs/exec_procs -run '^TestHostPolicyNativeEnforcement$' -count=1 -v
go test ./protocol/ui ./libs/host
node --test desktop/ui/*.test.mjs desktop/browser/*.test.mjs
```

macOS 本轮真实探测覆盖允许读取/写入、只读写入失败、白名单外读取失败、deny 的读写失败，全部使用临时文件。Linux/Windows 本轮完成交叉编译，没有真机执行验证。
