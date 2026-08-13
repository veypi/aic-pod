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
