package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/fsauth"
	"github.com/veypi/aic-pod/libs/proto"
)

// grant_apply（v0.14.5 §3）：文件权限白名单申请。
//
//	exec grant_apply <path> [--temp|--permanent]
//
// required 4（vcore 分级表）⇒ 必人工审批；批准后 granted 9 到达本函数。
//   - --temp（默认）：Policy.Grant(sid)——host 进程内存，重启失效、跨 session 失效；
//     不追溯已启动的 bg 任务（沙箱白名单在 Start 时固化）。
//   - --permanent：写 cfg fs_write_roots（基于文件配置修改 + Save 落盘，
//     与 set_config 同路径）——重启/跨 session 生效。
//   - deny_paths 内的路径拒绝申请（Policy.DenyHit 校验，canonical 判定）。
func (c *Client) runGrantApply(sid, msgID string, argv []string) *proto.ToolResponse {
	path, permanent, err := parseGrantArgv(argv)
	if err != nil {
		return &proto.ToolResponse{MsgID: msgID, State: proto.StateError, Error: "exec grant_apply: " + err.Error()}
	}
	abs, err := filepath.Abs(expandHomeDir(path))
	if err != nil {
		return &proto.ToolResponse{MsgID: msgID, State: proto.StateError,
			Error: fmt.Sprintf("exec grant_apply: invalid path %q: %v", path, err)}
	}
	if c.policy.DenyHit(abs) {
		return &proto.ToolResponse{MsgID: msgID, State: proto.StateRejected,
			Error: fmt.Sprintf("exec grant_apply: %s is in the deny list and cannot be granted", abs)}
	}

	scope := "session"
	if permanent {
		if err := c.grantPermanent(abs); err != nil {
			return &proto.ToolResponse{MsgID: msgID, State: proto.StateError,
				Error: "exec grant_apply: persist: " + err.Error()}
		}
		scope = "permanent"
	} else {
		c.policy.Grant(sid, abs)
	}

	roots := c.policy.WriteRootsFor(sid)
	return &proto.ToolResponse{
		MsgID: msgID, State: proto.StateCompleted,
		Content: fmt.Sprintf("granted write access: %s (scope=%s, applies to fs writes and sandbox write binds)\ncurrent writable roots (%d):\n%s",
			abs, scope, len(roots), strings.Join(roots, "\n")),
		Attrs: map[string]string{"action": "grant_apply", "path": abs, "scope": scope},
	}
}

// parseGrantArgv 解析 grant_apply 参数：<path> [--temp|--permanent]（默认 --temp）。
func parseGrantArgv(argv []string) (path string, permanent bool, err error) {
	for _, a := range argv {
		switch a {
		case "--temp":
			permanent = false
		case "--permanent":
			permanent = true
		default:
			if strings.HasPrefix(a, "-") {
				return "", false, fmt.Errorf("unknown flag %q (supported: --temp, --permanent)", a)
			}
			if path != "" {
				return "", false, fmt.Errorf("multiple paths given: %q and %q", path, a)
			}
			path = a
		}
	}
	if strings.TrimSpace(path) == "" {
		return "", false, fmt.Errorf("path is required (usage: grant_apply <path> [--temp|--permanent])")
	}
	return path, permanent, nil
}

// grantPermanent 把路径追加进 cfg fs_write_roots 并落盘（基于文件配置修改——
// flag/env 启动覆盖不落盘，与 api.SetConfig 同语义）；幂等（canonical 口径下
// 已存在跳过——macOS /var → /private/var 类 symlink 不再产生重复条目）。
func (c *Client) grantPermanent(abs string) error {
	fileCfg, err := cfg.LoadFile()
	if err != nil {
		return err
	}
	want := fsauth.Canonical(abs)
	for _, p := range fileCfg.FsWriteRoots {
		if fsauth.Canonical(expandHomeDir(p)) == want {
			c.policy.Reconcile() // 确保内存表已含该条目
			return nil
		}
	}
	fileCfg.FsWriteRoots = append(fileCfg.FsWriteRoots, abs)
	if err := cfg.Save(fileCfg); err != nil {
		return err
	}
	cfg.SetFsRoots(fileCfg.FsWriteRoots, fileCfg.FsDenyPaths)
	c.policy.Reconcile()
	return nil
}

// expandHomeDir 展开路径的 ~ 前缀（与 api 包 expandHome 同语义；grant_apply
// 面向 AI 直接调用，落盘前展开为真实绝对路径）。展开失败原样返回（后续
// Abs/canonical 会处理或报错）。
func expandHomeDir(p string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, strings.TrimPrefix(p, "~/"))
	}
	return p
}
