package fsauth

// denyCommon 返回三平台路径形态完全一致的通用拒绝条目（开发工具链凭证，
// 各平台位置相同）。平台特有路径（系统凭证库/浏览器 profile/容器运行时等）
// 在 deny_{darwin,linux,windows,other}.go 的 defaultDenyPaths 里各自叠加——
// 初始名单按平台分表（v0.14.5 评审二轮：三平台相关路径不同，分表消除
// 跨平台变量展开串扰风险；用户 cfg fs_deny 仍在本平台并集叠加）。
//
// 条目为 glob（支持跨段 **；大小写按平台折叠）；~/$VAR/%VAR%/$UserConfigDir
// 在 compileDeny 预展开时展开。平台上不存在的条目惰性失配无害（模式照常
// 编译、无命中）——分表原则只约束需要变量展开的平台特有路径。
func denyCommon() []string {
	return []string{
		"**/.ssh/**",
		"**/id_rsa*", "**/id_ed25519*", "**/id_ecdsa*", "**/id_dsa*",
		"~/.aws/**", "~/.config/gcloud/**", "~/.azure/**",
		"~/.netrc", "~/.npmrc", "~/.docker/config.json",
		"~/.claude/**", "~/.config/gh/**", "~/.config/opencode/**", "~/.codex/**", "~/.gnupg/**",
		"**/.git-credentials", "**/.env", "~/**/*.pem", "**/*.key",
		// shell 命令历史（常含粘贴的凭据/密钥）
		"~/.zsh_history", "~/.bash_history", "~/.python_history", "~/.node_repl_history",
		// 容器守护进程 socket：connect = 完全控制守护进程 = 主机逃逸
		// （挂载宿主根/特权容器）。通配覆盖厂商特化形态（OrbStack symlink
		// 目标、podman machine 等）；unix connect 不走 file-* 判定（实测
		// 2026-09-05），darwin 须配 network-outbound 规则（见 sandbox.go
		// seatbeltArgs）、linux 须 socket 覆盖挂载（见 overlayArgs）。
		"**/docker.sock", "**/podman.sock", "**/containerd.sock",
		"/var/run/docker.sock",
		// browser state 目录级拒绝：全量 cookie 库 browser.json 及其
		// 保存流程临时文件（.cli-tmp/.merge-tmp，含同等全量状态）一并覆盖。
		"$HOME/.aic/.cache/browser/**",
		"$UserConfigDir/aic/config.yaml",
	}
}
