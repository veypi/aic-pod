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

// WriteGrade 分级：全部命中 → 2；任一路径在全部根外 → 3；
// 空名单恒 2（分级关闭）；空路径集 2；空串路径跳过。
func TestWriteGrade(t *testing.T) {
	roots := []string{"/u/alice/sessions/s1"}
	if g := WriteGrade([]string{"/u/alice/sessions/s1/a", "/u/alice/sessions/s1/b"}, roots); g != LevelWrite {
		t.Errorf("all inside roots: grade = %d, want %d", g, LevelWrite)
	}
	if g := WriteGrade([]string{"/u/alice/sessions/s1/a", "/u/alice/other"}, roots); g != LevelDanger {
		t.Errorf("one outside roots: grade = %d, want %d", g, LevelDanger)
	}
	if g := WriteGrade([]string{"/u/alice/other"}, roots); g != LevelDanger {
		t.Errorf("outside roots: grade = %d, want %d", g, LevelDanger)
	}
	// 空名单 = 分级关闭（单源钉死语义，两侧一致）
	if g := WriteGrade([]string{"/anywhere"}, nil); g != LevelWrite {
		t.Errorf("empty roots: grade = %d, want %d", g, LevelWrite)
	}
	if g := WriteGrade(nil, roots); g != LevelWrite {
		t.Errorf("empty paths: grade = %d, want %d", g, LevelWrite)
	}
	if g := WriteGrade([]string{""}, roots); g != LevelWrite {
		t.Errorf("blank path skipped: grade = %d, want %d", g, LevelWrite)
	}
}
