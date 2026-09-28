package proto

import "testing"

// InWriteRoots 前缀判定：等于/位于其下命中；互为前缀不命中；空根跳过；尾斜杠容忍。
func TestInWriteRoots(t *testing.T) {
	roots := []string{"/u/alice/sessions/s1", "/tmp/x/", ""}
	cases := []struct {
		name string
		want bool
	}{
		{"/u/alice/sessions/s1", true},         // 等于根
		{"/u/alice/sessions/s1/a/b.txt", true}, // 根下
		{"/tmp/x", true},                       // 尾斜杠根的归一等值
		{"/tmp/x/f", true},                     // 尾斜杠根下
		{"/u/alice/sessions/s11", false},       // 互为前缀不命中（段边界）
		{"/u/alice/sessions", false},           // 根的父目录不命中
		{"/u/alice/sessions/s1.txt", false},    // 前缀 + 非段边界
		{"/etc/passwd", false},                 // 无关路径
	}
	for _, c := range cases {
		if got := InWriteRoots(c.name, roots); got != c.want {
			t.Errorf("InWriteRoots(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}
