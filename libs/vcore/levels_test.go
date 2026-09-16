package vcore

import (
	"testing"

	"github.com/veypi/aic-pod/libs/proto"
)

// git 子命令分级（§2.4）：常用写操作 Write，不可恢复/外发 Danger，
// checkout 的 pathspec 形态（--）单独提升。
func TestGitRequired(t *testing.T) {
	cases := []struct {
		argv []string
		want int
	}{
		{[]string{"status"}, proto.LevelRead},
		{[]string{"branch"}, proto.LevelRead},
		{[]string{"branch", "--list", "feat*"}, proto.LevelRead},
		{[]string{"branch", "new"}, proto.LevelWrite},
		{[]string{"branch", "-D", "old"}, proto.LevelDanger},
		{[]string{"log", "--oneline"}, proto.LevelRead},
		{[]string{"diff"}, proto.LevelRead},
		{[]string{"add", "."}, proto.LevelWrite},
		{[]string{"commit", "-m", "x"}, proto.LevelWrite},
		{[]string{"checkout", "main"}, proto.LevelWrite},
		{[]string{"checkout", "-b", "feat"}, proto.LevelWrite},
		{[]string{"switch", "main"}, proto.LevelWrite},
		{[]string{"push", "origin", "main"}, proto.LevelDanger},
		{[]string{"reset", "--hard", "HEAD~1"}, proto.LevelDanger},
		{[]string{"clean", "-fd"}, proto.LevelDanger},     // 未识别子命令兜底
		{[]string{"restore", "a.txt"}, proto.LevelDanger}, // 未识别子命令兜底
		// checkout pathspec 形态：丢弃工作区未提交修改 → Danger
		{[]string{"checkout", "--", "a.txt"}, proto.LevelDanger},
		{[]string{"checkout", "--", "."}, proto.LevelDanger},
		{[]string{"checkout", "HEAD~1", "--", "a.txt"}, proto.LevelDanger},
		// 无 -- 但明显是路径形态（不可能是合法 refname）→ Danger
		{[]string{"checkout", "."}, proto.LevelDanger},
		{[]string{"checkout", ".."}, proto.LevelDanger},
		{[]string{"checkout", "./src"}, proto.LevelDanger},
		{[]string{"checkout", "../x"}, proto.LevelDanger},
		{[]string{"checkout", "/abs"}, proto.LevelDanger},
		{[]string{"checkout", "src/"}, proto.LevelDanger},
		{[]string{"checkout", "a b"}, proto.LevelDanger},
		{[]string{"checkout", `C:\x`}, proto.LevelDanger},
		// 合法 refname/rev 形态保持 Write（~ 不在标记集：detached 安全）
		{[]string{"checkout", "HEAD~1"}, proto.LevelWrite},
		{[]string{"checkout", "-b", "feat/x"}, proto.LevelWrite},
		{[]string{"checkout", "-"}, proto.LevelWrite},
		// 带值 flag 跳过（-C/-c）：子命令判定不受其值干扰
		{[]string{"-C", "/repo", "checkout", "--", "x"}, proto.LevelDanger},
		{[]string{"-C", "/repo", "add", "."}, proto.LevelWrite},
	}
	for _, c := range cases {
		if got := gitRequired(c.argv); got != c.want {
			t.Errorf("gitRequired(%v) = %d, want %d", c.argv, got, c.want)
		}
	}
}

// cua 子命令分级（§2.4）：读类 Read，窗口交互 Write，
// --delivery foreground 提级 Danger；未知子命令 Danger 兜底。
func TestCuaRequired(t *testing.T) {
	cases := []struct {
		argv []string
		want int
	}{
		{[]string{"apps"}, proto.LevelRead},
		{[]string{"windows", "--pid", "1234"}, proto.LevelRead},
		{[]string{"snapshot", "--pid", "1234", "--png"}, proto.LevelRead},
		{[]string{"doctor"}, proto.LevelRead},
		{[]string{"clipboard", "read"}, proto.LevelRead},
		{[]string{"clipboard", "write", "hello"}, proto.LevelWrite},
		// cursor：agent 光标浮层控制全 Read（Windows 闪烁止血入口，纯视觉层）
		{[]string{"cursor", "off"}, proto.LevelRead},
		{[]string{"cursor", "on"}, proto.LevelRead},
		{[]string{"cursor", "state"}, proto.LevelRead},
		{[]string{"cursor", "motion", "--glide-duration-ms", "700"}, proto.LevelRead},
		{[]string{"cursor", "theme", "cua.default", "--reduced-motion", "off"}, proto.LevelRead},
		{[]string{"launch", "--app", "Notes"}, proto.LevelWrite},
		{[]string{"click", "--token", "t1"}, proto.LevelWrite},
		{[]string{"click", "--x", "100", "--y", "200"}, proto.LevelWrite},
		{[]string{"type", "--text", "hello"}, proto.LevelWrite},
		{[]string{"hotkey", "cmd+c"}, proto.LevelWrite},
		{[]string{"set-frame", "--pid", "1", "--window", "2", "--x", "0", "--y", "0", "--width", "800", "--height", "600"}, proto.LevelWrite},
		// front：前台激活（窃取前台焦点）→ Danger
		{[]string{"front", "--pid", "1234"}, proto.LevelDanger},
		// 前台投递 → Danger（任意位置出现都提级；--delivery-mode 为透传写法）
		{[]string{"click", "--token", "t1", "--delivery", "foreground"}, proto.LevelDanger},
		{[]string{"click", "--x", "1", "--y", "2", "--delivery-mode", "foreground"}, proto.LevelDanger},
		// --scope desktop = 真实物理指针（用户可见接管）→ Danger；window 不提级
		{[]string{"move", "--x", "1", "--y", "2", "--scope", "desktop"}, proto.LevelDanger},
		{[]string{"click", "--x", "1", "--y", "2", "--scope", "desktop"}, proto.LevelDanger},
		{[]string{"--scope", "desktop", "move", "--x", "1", "--y", "2"}, proto.LevelDanger},
		{[]string{"move", "--x", "1", "--y", "2", "--scope", "window"}, proto.LevelWrite},
		// 透传参数不干扰子命令判定（子命令恒为首个非 flag 元素）
		{[]string{"click", "--x", "1", "--y", "2", "--count", "2"}, proto.LevelWrite},
		// 未知子命令兜底
		{[]string{"explode"}, proto.LevelDanger},
		{[]string{}, proto.LevelDanger},
		// flag 值不干扰子命令判定
		{[]string{"--pid", "1234", "snapshot"}, proto.LevelRead},
		// typed browser 家族
		{[]string{"browser-state", "--pid", "1"}, proto.LevelRead},
		{[]string{"navigate", "--url", "https://example.com"}, proto.LevelWrite},
		{[]string{"bclick", "--ref", "e1"}, proto.LevelWrite},
		{[]string{"btype", "--ref", "e1", "--text", "x"}, proto.LevelWrite},
		{[]string{"bend"}, proto.LevelWrite},
		// bprepare：隔离 profile=Write；绑定用户真实浏览器=Danger
		{[]string{"bprepare", "--isolated"}, proto.LevelWrite},
		{[]string{"bprepare", "--pid", "1", "--window", "2"}, proto.LevelDanger},
		// run：JS 脚本执行恒 Danger（内容不可静态分级，脚本全文随审批可见）；
		// --code 值不干扰子命令判定
		{[]string{"run", "--code", "click(1,2)"}, proto.LevelDanger},
		{[]string{"run", "--file", "/tmp/a.js"}, proto.LevelDanger},
	}
	for _, c := range cases {
		if got := cuaRequired(c.argv); got != c.want {
			t.Errorf("cuaRequired(%v) = %d, want %d", c.argv, got, c.want)
		}
	}
}

// cua 注册声明（§6.3）：provider 注册白名单依赖 vcore.Decl 有元数据。
func TestCuaDecl(t *testing.T) {
	d, ok := Decl("cua")
	if !ok {
		t.Fatal("Decl(cua) not found")
	}
	if d.RequiredLevel != proto.LevelRead || d.Desc == "" || d.Help == "" {
		t.Errorf("Decl(cua) = %+v", d)
	}
}
