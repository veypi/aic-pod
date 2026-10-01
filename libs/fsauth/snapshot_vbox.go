package fsauth

// snapshot_vbox.go 是 fsauth 状态层 → vbox 纯 matcher 的桥（状态层留 pod，vbox 表在此发射）。vsh 引擎 host 端 FS 适配器消费
// 本快照；语义为 v4 first-wins 新行序（temp → cfg/permanent → builtin deny →
// 工作区元数据保护 → 便利根）；旧 decide 路径（last-wins + 便利根表外授写）
// 的语义映射规则：
//   - old: builtin deny 在前、cfg 在后，后命中者胜 → cfg 可覆盖 builtin deny；
//     new: cfg 行排在 builtin deny 之前，首命中即中——覆盖关系保持。
//   - old: 组内后命中者胜 → new: 组内反转（末行提首）。
//   - old: 便利根仅在表判定非 deny 时授写（不入表）→ new: 便利根 rw 行排表尾，
//     builtin deny 在其上——"便利不压 deny"保持。
//   - old: 未命中 effNone → fs_policy 兜底 → new: DefaultWrite = open?RW:Deny。
// grant.go DenyHit 拒批（session 硬底线）已按 2.7.4 于 M3c 删除——新行序下 temp
// 行在表头，天然压一切（含 builtin deny），语义由行序表达。
//
// 工作区元数据保护（.git/.aws）：2026-10-01 自 vbox 专路（protectedMetadataNames）
// 收敛为此处内置 ro 行——插在 builtin deny 之后、便利根之前：cfg/temp 行
// （更靠表头）可显式覆盖，"便利不压保护"；仅原生进程沙箱快照
// （SnapshotForNative）下发，进程内 FS 门（Snapshot）维持现状。

import (
	"os"
	"path/filepath"

	"github.com/veypi/aic-pod/libs/policy"
	"github.com/veypi/vbox"
)

// Snapshot 返回 sid 的 vbox 规则表（纯拼接，每次调用取当次值——grant/cfg
// 动态生效）。首命中生效；DefaultWrite 依 fs_policy。
func (p *Policy) Snapshot(sid string) vbox.FSRuleSet {
	return p.snapshotWith(sid, nil)
}

// SnapshotForNative 是原生命令的沙箱快照：Snapshot + 工作区元数据保护行
// （ro：<workdir>/.git、<workdir>/.aws，仅目录存在时；git 命令豁免）。
// workdir 为 OS 原生态（调用方已过 proto.HostPathToOS）；cmd 为命令名。
func (p *Policy) SnapshotForNative(sid, workdir, cmd string) vbox.FSRuleSet {
	return p.snapshotWith(sid, protectRows(workdir, cmd))
}

// protectedMetaNames 是工作区可写时仍保持只读的内置元数据子路径名。
// .git 防破坏仓库元数据/历史；.aws 的凭证助手指令可被配置为可执行
// （codex 1d804e91b 同理由），目录可写 ≈ 代码执行。
var protectedMetaNames = []string{".git", ".aws"}

// protectRows 生成工作区元数据保护行（ro）。git 命令豁免：.git 的写等级
// 由 git 命令自身规则承担（判据 = 命令名 basename，与旧 vbox isGitArgv 相同；
// bash -c "git ..." 不豁免）。仅存在的目录下发（不存在无可保护对象）。
func protectRows(workdir, cmd string) []fsRule {
	if workdir == "" {
		return nil
	}
	switch filepath.Base(cmd) {
	case "git", "git.exe":
		return nil
	}
	var out []fsRule
	for _, name := range protectedMetaNames {
		p := filepath.Join(workdir, name)
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			continue
		}
		if r, ok := compileFSRule(policy.EffectRO+":"+p, "builtin"); ok {
			out = append(out, r)
		}
	}
	return out
}

// snapshotWith 是 Snapshot/SnapshotForNative 的统一装配出口：protect 为
// 额外内置 ro 行（工作区元数据保护），插在 builtin deny 之后、便利根之前。
func (p *Policy) snapshotWith(sid string, protect []fsRule) vbox.FSRuleSet {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var rules []vbox.Rule
	// temp（表头）：sid 临时 grant，canonical 前缀。
	for _, g := range p.grants[sid] {
		rules = append(rules, vbox.Rule{Pattern: g, Effect: vbox.EffRW, Class: vbox.ClassTemp})
	}
	// cfg/permanent（组内反转：old last-wins → new first-wins）。
	for i := len(p.rules) - 1; i >= 0; i-- {
		r := p.rules[i]
		if r.src != "cfg" {
			continue
		}
		rules = appendVboxRule(rules, r)
	}
	// builtin deny（全 deny，组内顺序无关）。
	for _, r := range p.rules {
		if r.src != "builtin" {
			continue
		}
		rules = appendVboxRule(rules, r)
	}
	// 工作区元数据保护（native 快照）：内置 ro 行；cfg/temp 可覆盖、便利根不可压。
	for _, r := range protect {
		rules = appendVboxRule(rules, r)
	}
	// 便利根 rw（表尾）：baseRoots + 缓存候选 + 会话区。
	for _, root := range p.convenienceRoots(sid) {
		rules = append(rules, vbox.Rule{Pattern: root, Effect: vbox.EffRW, Class: vbox.ClassConvenience})
	}
	defaultWrite := vbox.EffDeny
	if p.openMode {
		defaultWrite = vbox.EffRW
	}
	// 直接字面构造：pats 已是 canonical 双形态（compileFSRule 产物），
	// 不再过 NewFSRuleSet 的 CanonicalPattern（glob 模式经二次归一会错位）。
	return vbox.FSRuleSet{Rules: rules, DefaultWrite: defaultWrite}
}

// appendVboxRule 展开一条 fsRule 的全部匹配形态为多行 vbox.Rule。
func appendVboxRule(out []vbox.Rule, r fsRule) []vbox.Rule {
	eff := vbox.EffRW
	switch r.eff {
	case effDeny:
		eff = vbox.EffDeny
	case effRO:
		eff = vbox.EffRO
	}
	class := vbox.ClassCfg
	if r.src == "builtin" {
		class = vbox.ClassBuiltin
	}
	for _, pat := range r.pats {
		out = append(out, vbox.Rule{Pattern: pat, Effect: eff, Class: class})
	}
	return out
}

// convenienceRoots 便利根（与 decideRootsLocked 同源：基底 + 缓存 + 会话区，
// 不含 temp grant——temp 已单独占表头）。
func (p *Policy) convenienceRoots(sid string) []string {
	roots := make([]string, 0, len(p.baseRoots)+len(p.decideCaches)+1)
	roots = append(roots, p.baseRoots...)
	roots = append(roots, p.decideCaches...)
	if sid != "" && p.sessionDir != "" {
		roots = append(roots, p.sessionDir+"/"+sid)
	}
	return dedupClean(roots)
}
