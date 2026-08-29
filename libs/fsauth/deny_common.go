package fsauth

// denyCommon 返回三平台路径形态完全一致的通用拒绝条目（开发工具链凭证，
// 各平台位置相同）。平台特有路径（系统凭证库/浏览器 profile 等）在
// deny_{darwin,linux,windows,other}.go 的 defaultDenyPaths 里各自叠加——
// 初始名单按平台分表（v0.14.5 评审二轮：三平台相关路径不同，分表消除
// 跨平台变量展开串扰风险；用户 cfg fs_deny_paths 仍在本平台并集叠加）。
//
// 条目为 glob（支持跨段 **；大小写按平台折叠）；~/$VAR/%VAR%/$UserConfigDir
// 在 compileDeny 预展开时展开。
func denyCommon() []string {
	return []string{
		"**/.ssh/**",
		"**/id_rsa*", "**/id_ed25519*", "**/id_ecdsa*", "**/id_dsa*",
		"~/.aws/**", "~/.config/gcloud/**", "~/.azure/**",
		"~/.netrc", "~/.npmrc", "~/.docker/config.json",
		"~/.claude/**", "~/.config/gh/**", "~/.config/opencode/**", "~/.codex/**", "~/.gnupg/**",
		"**/.git-credentials", "**/.env", "**/*.pem", "**/*.key",
		// browser state 目录级拒绝：全量 cookie 库 browser.json 及其
		// 保存流程临时文件（.cli-tmp/.merge-tmp，含同等全量状态）一并覆盖。
		"$HOME/.aic/.cache/browser/**",
		"$UserConfigDir/aic/config.yaml",
	}
}
