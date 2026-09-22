# Host 执行策略与原生沙箱

状态：2026-09-22 当前实现。云端审批策略与本地执行策略分别决策；完整协议见 aic 的 `docs/permission.md`。本地策略不能被云端批准覆盖，不保留旧配置兼容逻辑。fs 模型：读默认开放（除 deny），fs_allow 只授写——已废除 `ro:` 只读授权。

## 配置

```yaml
fs_policy: deny
fs_deny: ["/work/private/**"]
fs_allow: ["/work/project/**"]
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

统一判定顺序为 deny、覆盖本次访问的 allow、policy 默认值。deny 恒优先且为读写双拒（审批与临时 grant 不可绕过）；fs 读方向默认开放（除 deny），fs_allow 只授予写：裸路径覆盖子树，通配条目精确匹配，支持段内 `*`、跨层 `**` 和 `?`；`fs_policy` 只描述写方向：deny=仅写白名单，open=写除 deny 外全放。exec 使用命令名精确匹配或 `*`，不把命令正文作为配置规则；exec/net 的 open 默认允许、deny 默认拒绝。

net/ssh 以 `host:port` 精确匹配主机，端口可为数字或 `*`。IPv6 使用方括号。明确 deny 不能被更具体的 allow 覆盖。

默认 fs/ssh policy 为 deny，exec/net 为 open。工作区、Session 区、临时区、工具缓存与公共区装配为默认可写资源（内置便利根）；默认凭证保护名单与 fs_deny 合并后统一优先（读写双拒）。读方向不再装配任何白名单（读默认开放），也不再需要系统运行库/系统 CA 读根。调用参数 workdir 只决定工作目录，不授予该目录权限。

配置文件仅接受当前 snake_case 键。错误的授权 policy/列表不能回退为 open 或清空；业务格式错误保留原值，类型解析失败保留可见错误标记，整个文件损坏则将全部授权字段标为无效。设备工具通过 `cfg.CheckAuth` 拒绝调用，授权等级和临时 grant 均不能绕过；本地设置 API 继续可用。必须显式修正错误字段后才能保存，无关设置更新不会覆盖原文件，也不能清除环境变量/flag 中的错误授权。文件解析错误必须修复文件或通过设置 API 修复，不能依赖更高优先级参数掩盖。配置保存经 `ValidateAuth` 校验；本地设置 API 与永久 grant 使用同一配置更新锁，写盘成功后更新内存。

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

- macOS Seatbelt：默认放行（读开放）；写方向先整体关闭再按写白名单放行范围；随后按 fs 规则表序逐行输出（M3 行序映射，2026-09-23）——deny 行转 file-read*/file-write* 双拒 + unix socket 出站拒绝，后置 ro/rw 行转 allow（读 + unix socket 连通，写放行受等级门控；SBPL 后规则胜），cfg / permanent grant 的覆盖在内核真实生效。网络规则仅对 loopback 可以精确执行；不能表达的域名/IP 条目在启动前拒绝。
- Linux bubblewrap：整机只读绑定为读视图（fs_policy=open 且写级时改整机读写绑定），写白名单逐个可写绑定；deny 以覆盖挂载落地（目录 tmpfs 黑洞，文件/socket 以 /dev/null 覆盖；后挂载优先），形态不可实例化的 deny 模式（递归超预算、无字面前缀的全 glob）与无法实例化的可写 glob 在启动前拒绝执行。
- Windows：受限令牌 + ACL 写授权（工作区/缓存/追加根 standing ACE，私有临时目录 per-call）+ per-call deny ACE（随机 SID 加入 restricting list，对每个 deny 目标追加完全拒绝 ACE 并继承到子对象，进程结束后撤销）；对受限令牌本就不可达的对象跳过（语义等价），可达对象加不上 ACE 则拒绝执行。fs_policy=open 写级与网络规则无法用令牌模型表达，携带时在启动前拒绝。

Windows 文件服务单独由 hostfs 实现，与上述原生进程沙箱限制无关：支持文件读写、编辑、搜索、复制、移动和 curl 输出文件，遵守统一 fs 授权。读取拒绝 reparse point，提交和移动以固定父目录句柄执行，保留版本条件与原子禁止覆盖。

nosandbox 免沙箱执行不再叠加本地 fs/net 策略条件：请求级 nosandbox 经 dispatch 强制 Critical(4) 人工审批（granted 9 随签名下发）后直接执行；ssh/scp 内部管控调用与全局 no_sandbox 配置同属免沙箱来源。无法建立沙箱时仍拒绝运行（沙箱路径 fail-closed），不提供静默裸跑兜底。

资源限额、进程组终止、后台任务与输出处理保持原有实现。Windows 盘符虚拟根仍通过统一路径模块解析。

## 验证

```sh
GOCACHE=/tmp/aic-permission-go-cache go test ./... -skip '^TestCuaLive'
# host 包在平台自身沙箱内跑时有系统调用受限的环境性失败（EPERM），以 nosandbox 复核：
#   go test ./libs/host（nosandbox）
AIC_SANDBOX_PROBE=1 GOCACHE=/tmp/aic-permission-go-cache go test ./libs/exec_procs -run '^TestHostPolicy' -count=1 -v   # 需 nosandbox（嵌套 sandbox-exec 在沙箱内被拒）
go test ./protocol/ui ./libs/hostfs
node --test ui（aic 仓库，前端）
```

macOS 本轮真实探测覆盖：白名单外路径可读（读开放）、白名单内可写、白名单外写被拒、deny 读写双拒与字面拼写（/tmp/$TMPDIR）、Xcode shim 工具链可用。Linux/Windows 完成交叉编译（windows 另跑 vet），没有真机执行验证。
