# 托管 git（managed git）

状态：2026-10-11 A-MVP 能力已实施（darwin/linux）；2026-10-26 统一命令边界已落地。关联：[managed-ssh](managed-ssh.md)、[host 沙箱](../reference/host-sandbox.md)。

当前模型中，`gitx` 只作为 Pod 显式注册的 `git` 命令实现，不再拦截 vbox 支撑的
`/usr/bin/git`。`git` 与其他命令一样先通过 `exec_policy`，再由命令实现处理 ssh
传输类子命令：连接、认证与私钥留在 pod 侧。`/usr/bin/git` 则是独立的 vbox 支撑命令，
使用统一的 exec + FS/NET 模型，不读取 `ssh_policy`。

Pod `git` 命令的目标仍是：日常 clone/fetch/pull/push/ls-remote 不需要
`grant fs ~/.ssh`（rw、会话级、对所有进程暴露全部私钥）或 nosandbox（零隔离）。

```sh
grant ssh github.com:22        # 与 managed ssh 同一授权域、同一审批
git clone git@github.com:owner/repo.git
cd repo && git pull && git push
```

## 命令入口与内部分流

vsh 先解析出命令实例并统一执行 `exec_policy`。只有解析为 Pod `git` 命令的调用才进入
`gitx`；`/usr/bin/git` 等 vbox 支撑命令绝不进入该实现。

Pod `git` 命令内部按以下形态决定能否提供受管 SSH 传输：

1. 子命令属于 `clone/fetch/pull/push/ls-remote`；
2. 参数是白名单形态；
3. remote 是 ssh 形态（scp-like `[user@]host:path` 或 `ssh://`）。

不支持的形态按该命令自身契约明确报错或交其非 SSH 实现处理，不能隐式改走
`/usr/bin/git` 来规避策略。不同命令实例之间没有 fallback。Windows 本期不提供受管
Git SSH wrapper（其形态为 sh 脚本）。

## 收敛措施（pod 侧 NoSandbox 执行，与 managed ssh 同一信任级）

- 命令先通过统一的 `exec_policy`；传输授权再复用 `ssh_policy`：remote URL 解析出
  `[user@]host[:port]` 后复用 managed ssh 的目标解析（`~/.ssh/config` 别名、
  IdentityFile、`~/.aic/ssh_config` 覆盖）与目标检查；拒绝文案即
  `request grant ssh host:port`。`grant cmd git` 只修改 exec 准入，不代替传输策略。
- 一次性 GIT_SSH wrapper：pod 生成单次使用的 sh 脚本，丢弃 git 按 URL
  推导的全部 ssh 实参（`-o/-p/-4/-6/[user@]host`），把拨号目标钉回授权
  检查过的参数（含 BatchMode、known_hosts 钉死、IdentityFile 解析结果）。
  因此 config 里的 `url.insteadOf`、`core.sshCommand` 无法改变实际拨号
  目标——最多造成连接失败，不会连向未授权主机。
- 只信任系统拥有的 git（`/usr/bin/git`；与 trustedSSHBinary 同原则：
  homebrew 等用户可写前缀在 NoSandbox 下不受信）。Pod `git` 只在注册名 `git` 下调用，
  伪造的同名 `git` 二进制永远不会被 pod 侧执行。
- `core.hooksPath=/dev/null`（pre-push 等本地 hooks 不执行）、
  `GIT_TERMINAL_PROMPT=0`、`GIT_EDITOR=true`、ReplaceEnv 最小环境
  （HOME + LANG/LC_ALL/SSH_AUTH_SOCK 白名单透传，同 managed ssh）。
- 选项白名单，fail-closed：未列出选项报错退出 2，不静默切换到另一个命令实例
 （那既改变权限语义，也可能只以密钥错误收场）。显式拒绝：`-c/--config`（内联配置可改写
  core.sshCommand）、`--upload-pack/--receive-pack/--exec`（任意命令
  执行）、`--template`（可携带 hooks）、push 的 `-f/--force/--mirror`
  与 `+` 前缀 refspec（破坏性；确需强推由用户显式 nosandbox）、
  `--all/--multiple/--recurse-submodules`（多 remote/子模块传输超出一个
  host pin 的覆盖范围）、`--edit`（无交互编辑器）。
- remote 显式化：`git pull`（无参数）等缺省 remote 场景，受管层先读
  `.git/HEAD` 与 `branch.<name>.remote`（回退 `origin`）解析出名字并
  显式传给 pod 侧 git——保证「授权的 host = 拨号的 host」，不依赖
  git 自己的分支配置选择。

## 边界与已知限制

- ssh 域范围仍是 host+port，不限制仓库路径与 ref 操作（同 managed ssh
  不限制远端命令）。force push 等破坏性语义靠选项白名单收敛，不做 ref
  级策略。
- pod 侧 git 读用户 `~/.gitconfig`（identity、insteadOf 等语义需要），
  攻击面由 wrapper 钉扎 + hooksPath + 白名单收敛；不锁 `-F none` 式全隔离。
- 仓库定位要求 `.git` 是目录（worktree/submodule 的 .git 文件布局报错）。
- `include.path` 不展开；`url.insteadOf` 不重写给授权判定（判定基于
  config 字面 url，拨号被 wrapper 钉在同一 host）。
- 本功能不提供全设备 git egress 隔离：`/usr/bin/git`、改名程序、IDE 内置 git、
  `GIT_SSH_COMMAND` 环境直指定等路径不经此层。vbox 支撑命令只使用统一
  exec + FS/NET 模型，不设置 Git 专属拦截。
- https 传输的凭证代理（credential helper）与 Windows 接入是后续阶段；
  ssh-agent 沙箱内转发（SSH_AUTH_SOCK 注入）作为方案 B 保留。

## 验证

单元：参数白名单/拒绝矩阵、remote 分类（scp-like/ssh://https/本地）、
.git/config 与 HEAD 解析、wrapper 引用与钉扎内容、未授权拒绝文案、
统一 exec 准入、命令实例独立性、退出码透传、临时文件清理。
集成：loopback 内存 sshd + 真实 git-upload-pack/git-receive-pack，
覆盖未授权零拨号、clone/pull（缺省 remote 跟随分支配置）/push/ls-remote、
Pod `git` 与 `/usr/bin/git` 不互相改道、危险选项 fail-closed。
Linux 需在目标平台重跑；Windows 接入后补实测。
